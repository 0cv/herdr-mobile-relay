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
        or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted" or os.getuid() == 0 or sys.version_info < (3, 11)):
    raise SystemExit("service dispatch fixtures require disposable GitHub-hosted CI")

import hashlib
import json
import pathlib
import plistlib
import shutil
import signal
import subprocess
import tempfile
import time
import tomllib

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

# Native transaction capture faults only: no hand-copied production observer.
# Payloads/statuses are literal finite DATA, separate from actual model activity.
def native_resource_snapshot():
    paths = ("config/relay.env", "config/github-token",
             ".config/systemd/user/herdr-mobile-relay.service", ".config/systemd/user/herdr-remote.service",
             "Library/LaunchAgents/com.herdr-mobile-relay.service.plist", "Library/LaunchAgents/com.herdr-remote.service.plist")
    return {p: [ (root / p).read_bytes().hex(), (root / p).stat().st_mode & 0o7777 ]
            if (root / p).exists() else None for p in paths}

def native_recovery_snapshot():
    result = {}
    for directory in (root / "state").glob("**/herdr-native-recovery.*"):
        for path in (directory, *sorted(directory.rglob("*"))):
            assert not path.is_symlink()
            result[str(path.relative_to(root))] = [path.read_bytes().hex() if path.is_file() else None,
                                                  path.stat().st_mode & 0o7777]
    return result

def arm_native_capture():
    if not (root / "native-capture-armed").exists():
        path = root / "native-armed-snapshot.json"
        path.write_text(json.dumps(native_resource_snapshot())); path.chmod(0o600)
        (root / "native-capture-armed").touch()
        event("native-rollback-armed")

def native_capture(seam, key, payload, status=0, stderr=False):
    control = root / "native-capture.json"
    if control.exists():
        config = json.loads(control.read_text())
        assert set(config) == {"seam", "label", "when", "at", "hex", "status"}
        assert config["seam"] in ("systemd", "domain", "label", "disabled")
        assert config["label"] in ("current", "legacy") and config["when"] in ("initial", "rollback", "armed")
        assert type(config["at"]) is int and 1 <= config["at"] <= 100
        assert type(config["status"]) is int and 0 <= config["status"] <= 255
        assert len(config["hex"]) <= 2 * 65537
        counts_path = root / "native-capture-counts.json"
        counts = json.loads(counts_path.read_text()) if counts_path.exists() else {}
        slot = seam + "/" + key; counts[slot] = counts.get(slot, 0) + 1
        counts_path.write_text(json.dumps(counts)); counts_path.chmod(0o600)
        armed = config["when"] == "initial" or (root / "native-capture-armed").exists()
        hit = (armed and seam == config["seam"] and key in ("all", config["label"])
               and counts[slot] >= config["at"])
        event("native-observation", seam=seam, label=key, number=counts[slot], fault=hit)
        if hit:
            payload = bytes.fromhex(config["hex"]); status = config["status"]
            marker = root / "native-capture-fault"
            if not marker.exists():
                path = root / "native-fault-snapshot.json"
                path.write_text(json.dumps(native_resource_snapshot())); path.chmod(0o600)
                path = root / "native-fault-recovery.json"
                path.write_text(json.dumps(native_recovery_snapshot())); path.chmod(0o600)
                marker.touch()
            event("native-capture-fault", seam=seam, label=key, status=status, bytes=len(payload))
    (sys.stderr.buffer if stderr else sys.stdout.buffer).write(payload)
    (sys.stderr.buffer if stderr else sys.stdout.buffer).flush()
    raise SystemExit(status)

def worker(kind):
    def stop(signum, frame):
        event(kind, action="stop")
        raise SystemExit(0)
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    event(kind, action="start", pid=os.getpid(), pointer=os.getenv("HERDR_GITHUB_TOKEN_FILE", ""),
          internal_pointer=os.getenv("RELAY_UPDATER_TOKEN_FILE", ""),
          raw=os.getenv("GH_TOKEN", "") + os.getenv("GITHUB_TOKEN", ""),
          rearm=os.getenv("HERDR_RELAY_REARM_BOOTSTRAP", ""))
    mode = os.getenv("EXIT_CHILD", "")
    if mode == kind:
        deadline = time.monotonic() + 10
        while not (root / ("exit-" + kind)).exists():
            assert time.monotonic() < deadline, "fixture exit handshake timed out"
            time.sleep(.02)
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
        if os.getenv("NATIVE_FAILURE") == "public-capture": arm_native_capture()
        raise SystemExit(1 if os.getenv("FAILURE") in ("app", "bundle", "trust") else 0)
    if args == ["tailscale-cli", "activation-check"]:
        event("cli-gate")
        if "ACTIVATION_RESULT" in os.environ:
            raise SystemExit(int(os.environ["ACTIVATION_RESULT"]))
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
    print(os.getuid())
    raise SystemExit(0)
if name == "uname":
    assert args == ["-s"]
    print(os.environ["FIXTURE_OS"])
    raise SystemExit(0)
if name == "curl":
    url = args[-1]
    public = url == "https://relay.example.test/readyz"
    assert public or url.startswith(("http://127.0.0.1:18375/", "http://127.0.0.1:8375/")), "unexpected network query"
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
    native_failure = os.getenv("NATIVE_FAILURE", "")
    definition = root / (".config/systemd/user/herdr-mobile-relay.service" if os.environ["FIXTURE_OS"] == "Linux" else "Library/LaunchAgents/com.herdr-mobile-relay.service.plist")
    replacement = definition.exists() and definition.read_text() != "previous definition\n"
    if (root / "replacement-active").exists() and replacement and native_failure.startswith("ready-"):
        failure = "readiness"
    if replacement and native_failure == "cancel":
        (root / "cancel-gate").touch()
        deadline = time.monotonic() + 10
        while not (root / "cancel-release").exists():
            assert time.monotonic() < deadline, "cancellation gate deadline"
            time.sleep(.02)
    if replacement and (root / "replacement-active").exists() and native_failure == "ready-capture":
        arm_native_capture(); failure = "readiness"
    if public and replacement and (root / "replacement-active").exists() and native_failure == "public-capture":
        arm_native_capture(); failure = "readiness"
    if failure == "readiness" or (public and failure in ("app", "bundle", "trust")):
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
    statefile = root / "activation"
    state = json.loads(statefile.read_text())
    labels = ("herdr-mobile-relay.service", "herdr-remote.service")
    domain = "gui/" + str(os.getuid())
    launch_labels = ("com.herdr-mobile-relay.service", "com.herdr-remote.service")
    targets = tuple(domain + "/" + label for label in launch_labels)
    # Optional NATIVE_ORIGIN, a closed list `<current|legacy>:<kind>`, injects a same-label
    # service whose definition origin is NOT Herdr's own expected path. UNSET (the default)
    # changes nothing: every origin answer below then matches the expected Herdr definition.
    # PRESENCE, not truthiness, selects validation: a variable that is set but empty (or
    # otherwise malformed) is a forbidden input, never silently treated as unset, so a
    # malformed fault-injection cell cannot quietly stop exercising its fault.
    origin_role, origin_kind = "", ""
    if "NATIVE_ORIGIN" in os.environ:
        origin_role, _, origin_kind = os.environ["NATIVE_ORIGIN"].partition(":")
        known = (("foreign", "dropin", "empty", "duplicate", "fail", "oversized", "blank") if name == "systemctl"
                 else ("foreign", "empty", "duplicate", "malformed", "malformed-space", "malformed-tab"))
        if origin_role not in ("current", "legacy") or origin_kind not in known:
            event("forbidden", tool=name, point="native-origin-input"); raise SystemExit(99)
    def origin_for(role):
        return origin_kind if origin_role == role else ""
    if name == "systemctl":
        valid = (args == ["--user", "show-environment"] or args == ["--user", "daemon-reload"]
                 or (len(args) == 6 and args[:2] == ["--user", "show"] and args[2] in labels
                     and args[3:] == ["--property=LoadState", "--property=ActiveState", "--property=UnitFileState"])
                 or (len(args) == 5 and args[:2] == ["--user", "show"] and args[2] in labels
                     and args[3:] == ["--property=FragmentPath", "--property=DropInPaths"])
                 or (len(args) == 3 and args[0] == "--user" and args[1] in ("enable", "disable", "restart", "stop") and args[2] in labels)
                 or (len(args) == 4 and args[0] == "--user" and args[1] in ("is-active", "is-enabled") and args[2] == "--quiet" and args[3] in labels)
                 or (len(args) == 4 and args[:3] == ["--user", "disable", "--now"] and args[3] in labels))
    else:
        definitions = tuple(str(root / "Library/LaunchAgents" / (label + ".plist")) for label in launch_labels)
        valid = ((len(args) == 2 and args[0] == "print" and args[1] in (domain, *targets))
                 or args == ["print-disabled", domain]
                 or (len(args) == 2 and args[0] in ("enable", "disable", "bootout") and args[1] in targets)
                 or (len(args) == 3 and args[:2] == ["bootstrap", domain] and args[2] in definitions)
                 or (len(args) == 3 and args[:2] == ["kickstart", "-k"] and args[2] in targets))
    if not valid:
        event("forbidden", tool=name, argv=args); raise SystemExit(99)
    event("manager", tool=name, argv=args, command=args[1] if name == "systemctl" else args[0])
    if name == "systemctl":
        command = args[1]
        label = args[-1]
        key = "legacy" if label == "herdr-remote.service" else "current"
        if command == "show":
            key = "legacy" if args[2] == "herdr-remote.service" else "current"
            if len(args) == 5:
                # Additive origin query: its own exact shape, never a native_capture seam and
                # never counted, so no existing per-seam counter or fault control can shift.
                kind = origin_for(key)
                if kind == "fail": raise SystemExit(2)
                expected = root / ".config/systemd/user" / args[2]
                fragment = str(expected) if expected.exists() else ""
                overlays = ""
                if kind in ("dropin", "duplicate"): fragment = str(expected)
                if kind == "dropin": overlays = str(expected) + ".d/override.conf"
                if kind == "foreign": fragment = "/usr/lib/systemd/user/" + args[2]
                if kind == "empty": fragment = ""
                if kind == "oversized": fragment = "x" * 5000
                lines = ["FragmentPath=" + fragment] + (["FragmentPath=" + fragment] if kind == "duplicate" else []) + ["DropInPaths=" + overlays]
                # `blank` (systemd only): the otherwise-matching reply plus ONE extra newline, i.e. a trailing blank record.
                sys.stdout.write("\n".join(lines) + "\n" + ("\n" if kind == "blank" else "")); sys.stdout.flush()
                raise SystemExit(0)
            if os.getenv("NATIVE_FAILURE") == "query" or ((root / "replacement-active").exists() and os.getenv("NATIVE_FAILURE") in ("ready-unknown", "public-unknown")):
                raise SystemExit(2)
            path = root / ".config/systemd/user" / args[2]
            # A genuine foreign service is loaded even when no Herdr definition exists at the
            # expected path; with NATIVE_ORIGIN unset this is exactly path.exists() as before.
            loaded = path.exists() or origin_for(key) == "foreign"
            payload = ("LoadState=" + ("loaded" if loaded else "not-found") + "\n"
                       + "ActiveState=" + ("active" if state[key]["active"] else "inactive") + "\n"
                       + "UnitFileState=" + (("enabled" if state[key]["enabled"] else "disabled") if loaded else "") + "\n")
            native_capture("systemd", key, payload.encode())
        if command in ("is-active", "is-enabled"):
            value = state[key]["active" if command == "is-active" else "enabled"]
            raise SystemExit(0 if value else (3 if command == "is-active" else 1))
        if command in ("show-environment", "daemon-reload"):
            if command == "daemon-reload" and os.getenv("NATIVE_FAILURE") == "reload" and not (root / "injected").exists():
                (root / "injected").touch(); event("fault", point="reload"); raise SystemExit(1)
            raise SystemExit(0)
        assert command in ("enable", "disable", "restart", "stop")
        failure = os.getenv("NATIVE_FAILURE", "")
        if command == "stop" and (failure == "stop" or ((root / "replacement-active").exists() and failure in ("ready-stop-fail", "ready-still-active", "public-stop-fail"))):
            raise SystemExit(0 if failure == "ready-still-active" else 1)
        if command in ("enable", "disable"): state[key]["enabled"] = command == "enable"
        if command in ("restart", "stop") or "--now" in args: state[key]["active"] = command == "restart"
    else:
        command = args[0]
        key = "legacy" if "com.herdr-remote.service" in args[-1] else "current"
        failure = os.getenv("NATIVE_FAILURE", "")
        if command == "print":
            if failure == "query" or ((root / "replacement-active").exists() and failure in ("ready-unknown", "public-unknown")):
                raise SystemExit(2)
            if args[1] == domain: native_capture("domain", "all", b"")
            kind = origin_for(key)
            malformed_paths = {"malformed": "\tpath=", "malformed-space": "\tpath =", "malformed-tab": "\tpath\t= "}
            if state[key]["active"] or kind in ("foreign", "empty", "duplicate") or kind in malformed_paths:
                # The loaded body carries ONE job-level, tab-indented `path =` line naming the
                # expected Herdr plist; the NATIVE_ORIGIN kinds below are the only variants.
                expected = str(root / "Library/LaunchAgents" / (args[1].rsplit("/", 1)[1] + ".plist"))
                body = args[1] + " = {\n state = running\n"
                if kind == "foreign": body += "\tpath = /Library/LaunchAgents/" + args[1].rsplit("/", 1)[1] + ".plist\n"
                elif kind != "empty": body += "\tpath = " + expected + "\n"
                if kind == "duplicate": body += "\tpath = " + expected + "\n"
                # F006: canonical expected record PLUS a conflicting malformed path key.
                # Each spelling is a separate variant so no correctly parsed duplicate can
                # mask acceptance of the malformed record under the old parser.
                if kind in malformed_paths:
                    body += malformed_paths[kind] + "/Library/LaunchAgents/" + args[1].rsplit("/", 1)[1] + ".plist\n"
                native_capture("label", key, (body + "}\n").encode())
            payload = ('Could not find service "' + args[1].rsplit("/", 1)[1] + '" in domain for user gui: ' + str(os.getuid()) + "\n").encode()
            native_capture("label", key, payload, 113, stderr=True)
        if command == "print-disabled":
            payload = "disabled services = {\n"
            for k, label in (("current", "com.herdr-mobile-relay.service"), ("legacy", "com.herdr-remote.service")):
                payload += ' "' + label + '" => ' + ("false" if state[k]["enabled"] else "true") + "\n"
            native_capture("disabled", "all", (payload + "}\n").encode())
        if command in ("enable", "disable"):
            state[key]["enabled"] = command == "enable"
        else:
            assert command in ("bootstrap", "bootout", "kickstart")
            if command == "bootout" and (failure == "stop" or ((root / "replacement-active").exists() and failure in ("ready-stop-fail", "ready-still-active", "public-stop-fail"))):
                raise SystemExit(0 if failure == "ready-still-active" else 1)
            assert command != "bootstrap" or not state[key]["active"], "competing launchd bootstrap"
            state[key]["active"] = command != "bootout"
    temporary = statefile.with_suffix(".pending")
    temporary.write_text(json.dumps(state)); temporary.replace(statefile)
    if command in ("restart", "bootstrap") and key == "current":
        definition = root / (".config/systemd/user/herdr-mobile-relay.service" if name == "systemctl" else "Library/LaunchAgents/com.herdr-mobile-relay.service.plist")
        if definition.exists() and definition.read_text() != "previous definition\n":
            (root / "replacement-active").touch()
            if os.getenv("NATIVE_FAILURE") == "start" and not (root / "injected").exists():
                (root / "injected").touch(); event("fault", point="start"); raise SystemExit(1)
    raise SystemExit(0)
if name in ("cp", "mv", "rm", "chmod", "mktemp", "cat", "head"):
    # Owned filesystem adapters instrument actual tools, never manager effects.
    state = json.loads((root / "activation").read_text())
    live = {str(root / p) for p in ("config/relay.env", "config/github-token",
            ".config/systemd/user/herdr-mobile-relay.service", ".config/systemd/user/herdr-remote.service",
            "Library/LaunchAgents/com.herdr-mobile-relay.service.plist", "Library/LaunchAgents/com.herdr-remote.service.plist")}
    destinations = args if name in ("rm", "chmod") else args[-1:]
    for p in destinations:
        if name in ("cp", "mv", "rm", "chmod") and p in live:
            if (root / "native-capture.json").exists():
                event("native-live-attempt", tool=name, destination=p,
                      faulted=(root / "native-capture-fault").exists(), armed=(root / "native-capture-armed").exists())
                if (root / "native-capture-fault").exists():
                    event("forbidden", tool=name, point="mutation-after-unknown-native-capture"); raise SystemExit(97)
            affected = [state["legacy"]] if "herdr-remote" in p else list(state.values())
            assert not any(s["active"] for s in affected), "live-resource mutation before affected-label quiescence"
    failure = os.getenv("NATIVE_FAILURE", "")
    hit = ((failure == "candidate" and name == "cp" and args[-1].endswith("candidate.env"))
           or (failure == "snapshot" and name == "cp" and args[-1].endswith("/0"))
           or (failure == "chmod" and name == "chmod" and ".relay-env." in args[-1])
           or (failure == "token" and name == "chmod" and args[-1].endswith("candidate.token"))
           or (failure == "rename" and name == "mv" and args[-1] == str(root / "config/relay.env"))
           or (failure == "commit" and name == "rm" and any("herdr-remote" in p for p in args))
           or (failure == "ready-restore" and name == "cp" and args[-2].endswith("/0"))
           or (failure == "ready-restore" and name == "rm" and args[-1] in live and "herdr-mobile-relay.service" in args[-1]))
    if hit and not (root / "injected").exists():
        (root / "injected").touch(); event("fault", point=failure); raise SystemExit(1)
    if name == "head" and failure == "read" and args == ["-c", "262145", str(root / "config/relay.env")]:
        print("GH_TOKEN=partial-private", end=""); raise SystemExit(2)
    event("filesystem", tool=name, live=any(p in live for p in destinations))
    os.execv(("/usr/bin/" if name in ("mktemp", "head") else "/bin/") + name, [name, *args])
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
        # Darwin's disposable /var roots may alias /private/var. Fixtures
        # select the physical owned root rather than weakening native policy.
        self.root = parent.resolve() / name
        self.root.mkdir(mode=0o700)
        self.bin = self.root / ".local/bin"
        release = self.root / "releases/current"
        shutil.copytree(REPO / "relay", release / "relay")
        for entry in ("common.sh", "herdr-mobile-relay-service.sh", "tailscale-cli-service.sh",
                      "install-service.sh", "install-systemd-user-service.sh", "native-install-transaction.sh"):
            original, copied = REPO / "relay" / entry, release / "relay" / entry
            assert copied.read_bytes() == original.read_bytes()
            assert hashlib.sha256(copied.read_bytes()).digest() == hashlib.sha256(original.read_bytes()).digest()
            assert copied.stat().st_mode & 0o7777 == original.stat().st_mode & 0o7777
        (release / "web").mkdir()
        shutil.copyfile(REPO / "web/release.json", release / "web/release.json")
        write(release / "release-manifest.json", '{"version":"version","revision":"revision","web_hash":"web"}\n')
        fake = "#!" + sys.executable + "\n" + FAKE
        write(release / "herdr-mobile-relay", fake, True)
        for tool in ("curl", "cloudflared", "systemctl", "launchctl", "plutil", "systemd-analyze",
                     "id", "uname", "sleep", "tailscale", "tailscaled", "herdr", "ssh", "npx",
                     "wrangler", "security", "sudo", "launchd", "systemd", "go", "git",
                     "cp", "mv", "rm", "chmod", "mktemp", "cat", "head"):
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
        write(self.root / "config/devices.json", '{"synthetic_private_state":true}\n')
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
        if result.returncode == 0:
            # Parsed generated semantics only, never real manager behavior.
            if self.platform == "launchd":
                definition = plistlib.loads(self.definition.read_bytes())
                assert definition["KeepAlive"] == dict(SuccessfulExit=False)
                assert definition["ProgramArguments"] == [str(self.script.parent / "herdr-mobile-relay-service.sh"), "--launchd"]
                assert definition["RunAtLoad"] is True and definition["ThrottleInterval"] == 10
            else:
                unit = dict(line.split("=", 1) for line in self.definition.read_text().splitlines()
                            if "=" in line and not line.startswith("#"))
                assert unit["Type"] == "simple" and unit["Restart"] == "on-failure" and unit["RestartSec"] == "10"
                assert unit["RestartPreventExitStatus"].split() == ["69", "78"]
                assert "--launchd" not in unit["ExecStart"]
                assert not {"SuccessExitStatus", "RemainAfterExit"} & unit.keys()
        return result

    def wrapper(self, child="relay", code=23, termination=None, launchd=False):
        self.env.update(EXIT_CHILD="" if termination else child, CHILD_CODE=str(code),
                        GH_TOKEN=CANARIES[0], GITHUB_TOKEN=CANARIES[0])
        with self.envfile.open("a") as file:
            file.write(f"GH_TOKEN={CANARIES[1]}\nGITHUB_TOKEN={CANARIES[1]}\nHERDR_GITHUB_TOKEN_FILE='{self.root}/pointer'\n")
        argv = launchd_arguments(self.script.parent) if launchd else [str(self.script.parent / "herdr-mobile-relay-service.sh")]
        proc = subprocess.Popen(["/bin/bash", "-x", *argv],
                                env=self.env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, start_new_session=True)
        try:
            expected = {"relay", "tunnel"} if self.transport == "cloudflare" else {"relay"}
            deadline = time.monotonic() + 5
            while proc.poll() is None:
                started_kinds = {e["kind"] for e in self.events()
                                 if e["kind"] in ("relay", "tunnel") and e["action"] == "start"}
                if started_kinds == expected:
                    if termination:
                        proc.send_signal(termination)
                    else:
                        write(self.root / ("exit-" + child), "release\n")
                    break
                assert time.monotonic() < deadline, "essential children never started"
                time.sleep(.02)
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


def launchd_arguments(relay):
    # Parse the actual copied producer template as DATA, not an invented argv.
    source = (relay / "install-service.sh").read_text()
    template = source.split('cat > "$STAGED_PLIST" <<EOF\n', 1)[1].split("\nEOF", 1)[0]
    definition = plistlib.loads(template.encode())
    assert definition["KeepAlive"] == {"SuccessfulExit": False}
    assert definition["RunAtLoad"] is True and definition["ThrottleInterval"] == 10
    assert definition["ProgramArguments"] == ["$SERVICE_WRAPPER_XML", "--launchd"]
    return [str(relay / "herdr-mobile-relay-service.sh"), *definition["ProgramArguments"][1:]]


def unavailable_snapshot(root):
    snapshot = {}
    for path in [root, *sorted(root.rglob("*"))]:
        if path == root / "events":
            continue  # only append-only, credential-free adapter observations
        mode = path.lstat().st_mode & 0o7777
        if path.is_symlink():
            value = ("link", mode, os.readlink(path))
        elif path.is_dir():
            value = ("directory", mode)
        else:
            # Borrow owner-read for snapshots only, restoring before invocation.
            try:
                path.chmod(mode | 0o400)
                value = ("file", mode, path.read_bytes())
            finally:
                path.chmod(mode)
        snapshot[str(path.relative_to(root))] = value
    return snapshot


def unavailable_run(argv, env):
    # A regression that accidentally starts children must not leak them on a
    # timeout. Containment targets only this owned invocation's process group.
    command = ["/bin/bash", "-x", *argv]
    proc = subprocess.Popen(command, env=env, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, start_new_session=True)
    try:
        stdout, stderr = proc.communicate(timeout=TIMEOUT)
        return subprocess.CompletedProcess(command, proc.returncode, stdout, stderr)
    finally:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        proc.wait(timeout=5)


def unavailable_cases(parent):
    assert os.getuid() != 0, "unreadable fixture requires the hosted non-root user"
    cli = "HERDR_RELAY_TRANSPORT=tailscale-cli\n"
    cells = [("valid", cli, {}, 69),
             ("absent", None, {"HERDR_RELAY_TRANSPORT": "tailscale-cli"}, 69),
             ("empty-mode", "HERDR_RELAY_TRANSPORT=''\n", {"HERDR_RELAY_TRANSPORT": " TAILSCALE-CLI "}, 69),
             ("missing-executable", cli + "HERDR_TAILSCALE_CLI_BIN='/missing-selected-cli'\n", {}, 69),
             ("missing-relay", cli, {"HERDR_RELAY_BIN": "/missing-selected-relay"}, 69),
             ("wrong-mode", "HERDR_RELAY_TRANSPORT=unknown\n", {}, 78),
             ("duplicate", cli + cli, {}, 69),
             ("conflicting", cli + "HERDR_RELAY_TRANSPORT=cloudflare\n", {}, 78),
             ("ambient-conflict", cli, {"HERDR_RELAY_TRANSPORT": "cloudflare"}, 78),
             ("ambient-invalid", cli, {"HERDR_RELAY_TRANSPORT": "unknown"}, 78),
             ("ambient-foreground", "HERDR_RELAY_TRANSPORT=''\n", {"HERDR_RELAY_TRANSPORT": "tailscale"}, 78),
             ("gateway-conflict", cli + "HERDR_GATEWAY_URL=''\n", {"HERDR_GATEWAY_URL": "wss://gateway.example.test"}, 78),
             ("foreground", "HERDR_RELAY_TRANSPORT=tailscale\n", {}, 78),
             ("external", "HERDR_RELAY_TRANSPORT=tailscale-external\n", {}, 78),
             ("unreadable", cli, {}, 78), ("implicit-legacy", cli, {}, 78),
             ("session", cli, {}, 78), ("dangling-external-session", cli, {}, 78),
             ("truncated-quote", cli + "HERDR_RELAY_TOKEN='unterminated\n", {}, 78),
             ("oversize", cli + "#" * 262145, {}, 78)]
    for key in ("PATH", "IFS", "BASH_ENV", "SHELLOPTS", "ENV_FILE", "SERVICE_LAUNCHD", "RELAY_UPDATER_TOKEN_FILE"):
        cells.append(("internal-" + key, cli + key + "='ignored'\n", {}, 78))
    for key in ("HERDR_RELAY_RUN_ID", "HERDR_RELAY_CONTROL_RUN_ID", "HERDR_RELAY_PAIRING_SOCKET",
                "HERDR_RELAY_REARM_BOOTSTRAP", "HERDR_TAILSCALE_REQUEST"):
        cells.append(("saved-" + key, cli + key + "=1\n", {}, 78))
        cells.append(("empty-" + key, cli + key + "=''\n", {key: "1"}, 78))
    for value in ("0", "false", "False", "FALSE", "f", "F", ""):
        cells.append(("false-" + (value or "empty"), cli + "HERDR_RELAY_REARM_BOOTSTRAP='" + value + "'\n", {}, 69))
    for bypass in ("development", "production"):
        cells.append(("bypass-" + bypass, cli + "HERDR_TAILSCALE_CLI_SCOPE='" + bypass + "'\n"
                      "HERDR_TAILSCALE_CLI_ALLOW_UNREGISTERED_START=1\n", {}, 69))
    # The marked activation adapter models both outcomes, but neither it nor a
    # CLI adapter may be called, even with bypass flags or a missing binary.
    for platform in ("systemd", "launchd"):
        for number, (name, text, ambient, direct_status) in enumerate(cells):
            f = Fixture(parent, f"unavailable-{platform}-{number}", "tailscale-cli", platform, initial=True)
            f.env.update(ambient, GH_TOKEN=CANARIES[0], GITHUB_TOKEN=CANARIES[0],
                         SERVICE_LAUNCHD="true", HERDR_TAILSCALE_CLI_ALLOW_UNREGISTERED_START="1")
            private = f.root / "config/github-token"
            write(private, CANARIES[1] + "\n")
            write(f.root / "config/push.env", "private-push-canary\n")
            write(f.root / "config/routes-reservations", "synthetic-retained\n")
            write(f.root / "config/bystander", "synthetic-protected\n")
            if text is None:
                f.envfile.unlink()
            else:
                write(f.envfile, text + "\n  export GH_TOKEN='" + CANARIES[1] + "'\n"
                      "GITHUB_TOKEN='" + CANARIES[1] + "'\nHERDR_GITHUB_TOKEN_FILE='" + str(private) + "'\n")
            if name == "unreadable": f.envfile.chmod(0)
            if name == "implicit-legacy":
                write(f.script.parent / ".env", cli)
                f.env.pop("HERDR_RELAY_ENV")
                f.env["HERDR_PLUGIN_CONFIG_DIR"] = str(f.root / "uncreated-plugin")
            if name == "session": write(f.root / "config/tailscale-session.env", "synthetic-session\n")
            if name == "dangling-external-session":
                (f.root / "config/tailscale-external-session.env").symlink_to(f.root / "missing-session")
            before = unavailable_snapshot(f.root)
            for activation in (0, 69):
                f.env["ACTIVATION_RESULT"] = str(activation)
                variants = [("cli", [str(f.script.parent / "tailscale-cli-service.sh")], 69),
                            ("direct", [str(f.script.parent / "herdr-mobile-relay-service.sh")], direct_status),
                            ("launchd", launchd_arguments(f.script.parent), 0)]
                for variant, argv, expected in variants:
                    result = unavailable_run(argv, f.env)
                    output = result.stdout + result.stderr
                    assert result.returncode == expected, (name, variant, result.returncode, output)
                    assert "unavailable/non-ready" in output and "Starting herdr" not in output
                    assert ("automatic retry suppressed" in output) == (variant == "launchd")
                    f.check_safe(output)
                    assert "private-push-canary" not in output
                    assert unavailable_snapshot(f.root) == before, (name, variant)
                    assert all(e["kind"] == "filesystem" and e["tool"] == "head" for e in f.events()), "refusal invoked a child, activation, or mutating tool"
        # Executable text is never evaluated, not even to recover policy data.
        f = Fixture(parent, "unavailable-eval-" + platform, "tailscale-cli", platform)
        marker = f.root / "evaluated"
        for text in (cli + "HERDR_RELAY_TOKEN=$(touch " + str(marker) + ")\n",
                     cli + "HERDR_RELAY_TOKEN=`touch " + str(marker) + "`\n",
                     cli + "HERDR_RELAY_TOKEN=x; touch " + str(marker) + "\n"):
            write(f.envfile, text)
            before = unavailable_snapshot(f.root)
            for argv, expected in (([str(f.script.parent / "herdr-mobile-relay-service.sh")], 78),
                                   (launchd_arguments(f.script.parent), 0),
                                   ([str(f.script.parent / "tailscale-cli-service.sh")], 69)):
                result = unavailable_run(argv, f.env)
                assert result.returncode == expected and not marker.exists()
                assert unavailable_snapshot(f.root) == before
                f.check_safe(result.stdout + result.stderr)
        for args in (("--unknown",), ("--launchd", "--launchd"), ("--launchd", "extra"), ("extra", "--launchd")):
            before = unavailable_snapshot(f.root)
            result = unavailable_run([str(f.script.parent / "herdr-mobile-relay-service.sh"), *args], f.env)
            assert result.returncode == 2 and "automatic retry suppressed" not in result.stderr
            assert unavailable_snapshot(f.root) == before
            f.check_safe(result.stdout + result.stderr)


# The public wizard has its own narrow adapter, not the native manager model.
# Its manifest entrypoint and helpers are actual copied source; only the stable
# child and potentially effectful tools are inert. No helper build occurs here.
WIZARD_FAKE = r'''
import json, os, pathlib, plistlib, shutil, signal, sys
root = pathlib.Path(os.environ["FIXTURE_ROOT"])
name, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
selected = pathlib.Path(os.environ["WIZARD_EXPECTED_ENV"])
fault = os.environ.get("WIZARD_FAULT", "")

def event(kind, **fields):
    with (root / "events").open("a") as out:
        out.write(json.dumps(dict(kind=kind, tool=name, **fields)) + "\n")

def inside(path):
    path = pathlib.Path(path)
    assert path.is_absolute() and root in path.parents, "tool escaped owned root"
    return path

def real():
    tool = shutil.which(name, path="/usr/bin:/bin:/usr/sbin:/sbin")
    assert tool, "required fixture file utility unavailable"
    os.execv(tool, [tool, *args])

if name == "stable-setup.sh":
    assert not args, "unexpected stable child argv"
    assert os.environ["HERDR_RELAY_ENV"] == str(selected)
    assert os.environ.get("HERDR_STABLE_SETUP_WRAPPED") == "1"
    assert os.environ.get("HERDR_RELAY_TRANSPORT") == "cloudflare"
    for key in ("HERDR_GATEWAY_URL", "HERDR_GATEWAY_SELECTION", "HERDR_TAILSCALE_ORIGIN",
                "HERDR_TAILSCALE_HTTPS_PORT", "HERDR_EXTERNAL_HTTPS_ORIGIN", "HERDR_RELAY_RUN_ID",
                "HERDR_RELAY_CONTROL_RUN_ID", "HERDR_RELAY_PAIRING_SOCKET",
                "HERDR_RELAY_REARM_BOOTSTRAP", "HERDR_TAILSCALE_REQUEST"):
        assert key not in os.environ, "stale inherited selection"
    text = selected.read_text()
    assert "HERDR_RELAY_TRANSPORT='cloudflare'\n" in text
    native_lines = json.loads(os.environ.get("WIZARD_NATIVE_LINES", "[]"))
    assert all(line in text for line in native_lines), "native-produced fields changed"
    if native_lines:
        assert os.environ.get("CLOUDFLARED_BIN") == os.environ.get("WIZARD_CALLER_BIN"), "saved routing was exported"
    # Record only presence, never credential bytes or the caller environment.
    event("delegate", env=str(selected), wrapped="1", transport="cloudflare",
          native_shape=bool(native_lines), raw_present=bool(os.getenv("GH_TOKEN") or os.getenv("GITHUB_TOKEN")))
    if os.getenv("WIZARD_CHILD_SIGNAL"):
        sig = getattr(signal, os.environ["WIZARD_CHILD_SIGNAL"])
        signal.signal(sig, signal.SIG_DFL)
        os.kill(os.getpid(), sig)
    raise SystemExit(int(os.getenv("WIZARD_CHILD_CODE", "0")))
if name == "uname" and args == ["-s"]:
    event("read")
    print(os.environ["FIXTURE_OS"])
    raise SystemExit(0)
if name == "id" and args == ["-u"]:
    event("read")
    print(os.getuid())
    raise SystemExit(0)
if name == "stat" and len(args) == 3 and args[:2] in (["-c", "%u:%a:%h"], ["-f", "%u:%Lp:%l"], ["-c", "%d:%i"], ["-f", "%d:%i"]):
    inside(args[2]); event("read")
    if fault == "stat": raise SystemExit(2)
    real()
if name == "plutil" and len(args) == 6 and args[:5] == ["-extract", "EnvironmentVariables.HERDR_RELAY_ENV", "raw", "-o", "-"]:
    path = inside(args[5]); event("decode")
    if fault == "decode": raise SystemExit(2)
    try:
        value = plistlib.loads(path.read_bytes())["EnvironmentVariables"]["HERDR_RELAY_ENV"]
        assert isinstance(value, str) and value
    except Exception:
        raise SystemExit(2)
    print(value)
    raise SystemExit(0)
if name == "wc" and args == ["-c"]:
    event("read")
    if fault == "wc": print("5"); raise SystemExit(2)
    real()
if name == "head" and len(args) == 3 and args[:2] == ["-c", "262145"]:
    path = inside(args[2]); event("read")
    if fault == "head" and str(path) == os.getenv("WIZARD_READ_TARGET", str(selected)):
        print("fixture-saved-token", end="")
        raise SystemExit(2)
    real()
if name == "mkdir" and args == ["-p", str(selected.parent)]:
    event("write", action="mkdir")
    real()
if name == "mktemp" and len(args) == 1 and args[0] == str(selected.parent / ".relay-env.XXXXXX"):
    event("write", action="mktemp")
    if fault == "mktemp": raise SystemExit(2)
    real()
if name == "chmod" and len(args) == 2 and args[0] == "600" and inside(args[1]).name.startswith(".relay-env."):
    event("write", action="chmod")
    if fault == "chmod": raise SystemExit(2)
    real()
if name == "mv" and len(args) == 3 and args[0] == "-f" and inside(args[1]).name.startswith(".relay-env.") and args[2] == str(selected):
    event("write", action="rename")
    candidate = pathlib.Path(args[1]).read_text()
    if fault == "mv": raise SystemExit(2)
    if fault.startswith("remove:"):
        key = fault.split(":", 1)[1] + "="
        if key in selected.read_text() and key not in candidate: raise SystemExit(2)
    if fault == "transport" and "HERDR_RELAY_TRANSPORT='cloudflare'\n" in candidate:
        raise SystemExit(2)
    real()
if name == "rm" and len(args) == 2 and args[0] == "-f" and inside(args[1]).name.startswith(".relay-env."):
    event("write", action="cleanup")
    real()
# cp is deliberately unsupported: no legacy env/push migration is permissible.
# Managers, installers, network, route tools, app/CLI children and unknown argv
# must refuse even if an unexpected wrapper branch reaches one of them.
event("forbidden", argc=len(args))
raise SystemExit(99)
'''


class WizardFixture:
    def __init__(self, parent, name, platform="Linux", text="HERDR_RELAY_TRANSPORT=cloudflare\n"):
        self.root = parent.resolve() / ("wizard-" + name)
        self.root.mkdir(mode=0o700)
        self.checkout = self.root / "checkout"
        self.relay = self.checkout / "relay"
        self.relay.mkdir(parents=True)
        for file in ("common.sh", "plugin-install-service.sh"):
            shutil.copyfile(REPO / "relay" / file, self.relay / file)
        shutil.copyfile(REPO / "herdr-plugin.toml", self.checkout / "herdr-plugin.toml")
        manifest = tomllib.loads((self.checkout / "herdr-plugin.toml").read_text())
        self.command = next(p["command"] for p in manifest["panes"] if p["id"] == "install-service")
        assert self.command == ["bash", "relay/plugin-install-service.sh"]
        fake = "#!" + sys.executable + "\n" + WIZARD_FAKE
        write(self.relay / "stable-setup.sh", fake, True)
        self.bin = self.root / "bin"
        for tool in ("uname", "id", "stat", "plutil", "wc", "head", "mkdir", "mktemp", "chmod", "mv", "rm", "cp",
                     "systemctl", "launchctl", "cloudflared", "tailscale", "tailscaled", "herdr", "curl", "ssh",
                     "sudo", "go", "git", "npx", "wrangler", "security", "herdr-mobile-relay"):
            write(self.bin / tool, fake, True)
        self.envfile = self.root / "selected/relay.env"
        if text is not None: write(self.envfile, text)
        self.definition = (self.root / ".config/systemd/user/herdr-mobile-relay.service" if platform == "Linux"
                           else self.root / "Library/LaunchAgents/com.herdr-mobile-relay.service.plist")
        # Protected synthetic state, credentials and legacy push material.
        write(self.root / "protected/devices.json", '{"synthetic":"stored-device"}\n')
        write(self.root / "protected/github-token", CANARIES[1] + "\n")
        write(self.root / "protected/activation", "bystander-active\n")
        write(self.relay / "push.env", "SYNTHETIC_PUSH=private-push-canary\n")
        self.env = dict(HOME=str(self.root), PATH=str(self.bin) + ":/usr/bin:/bin:/usr/sbin:/sbin",
                        HERDR_PLUTIL=str(self.bin / "plutil"),
                        FIXTURE_ROOT=str(self.root), FIXTURE_OS=platform, HERDR_RELAY_ENV=str(self.envfile),
                        WIZARD_EXPECTED_ENV=str(self.envfile), HERDR_SETUP_MENU="1",
                        XDG_STATE_HOME=str(self.root / "uncreated-state"),
                        XDG_CACHE_HOME=str(self.root / "uncreated-cache"),
                        XDG_DATA_HOME=str(self.root / "uncreated-data"))

    def select(self, path, explicit=True):
        self.envfile = path
        self.env["WIZARD_EXPECTED_ENV"] = str(path)
        if explicit: self.env["HERDR_RELAY_ENV"] = str(path)
        else: self.env.pop("HERDR_RELAY_ENV", None)

    def installed(self, path):
        if self.env["FIXTURE_OS"] == "Linux":
            write(self.definition, '[Service]\nEnvironment="HERDR_RELAY_ENV=' + str(path) + '"\n')
        else:
            self.definition.parent.mkdir(parents=True, exist_ok=True)
            self.definition.write_bytes(plistlib.dumps(dict(EnvironmentVariables=dict(HERDR_RELAY_ENV=str(path)))))
            self.definition.chmod(0o600)

    def native_cloudflare(self, binary=None):
        # Source-derived candidate.env shape from native_installer_candidate +
        # ensure_relay_env. This does not execute a native installer/producer.
        binary = self.bin / "cloudflared" if binary is None else binary
        config = self.root / "native/tunnel.yml"
        write(config, "hostname: relay.example.test\n")
        fields = (("CLOUDFLARED_BIN", str(binary)), ("CLOUDFLARED_CONFIG", str(config)),
                  ("HERDR_RELAY_PORT", "8375"), ("HERDR_RELAY_INSTANCE_ID", "fixture-native"),
                  ("HERDR_RELAY_TOKEN", "n" * 32),
                  ("HERDR_GITHUB_TOKEN_FILE", str(self.root / "protected/github-token")))
        lines = [key + "='" + value.replace("'", "'\\''") + "'\n" for key, value in fields]
        write(self.envfile, "HERDR_RELAY_TRANSPORT='cloudflare'\n" + "".join(lines))
        self.env["WIZARD_NATIVE_LINES"] = json.dumps(lines)

    def snapshot(self):
        result = {}
        for path in sorted(self.root.rglob("*")):
            if path == self.root / "events": continue
            mode = path.lstat().st_mode
            if path.is_symlink(): value = ("symlink", os.readlink(path))
            elif path.is_file():
                try: value = ("file", path.read_bytes())
                except PermissionError:
                    # Fixture-only byte inventory: borrow owner-read and restore
                    # the original mode before the actual wrapper can observe it.
                    assert path.stat().st_uid == os.getuid()
                    path.chmod((mode & 0o7777) | 0o400)
                    try: value = ("file", path.read_bytes())
                    finally: path.chmod(mode & 0o7777)
            elif path.is_dir(): value = ("directory",)
            else: value = ("special",)
            result[str(path.relative_to(self.root))] = (mode, value)
        return result

    def events(self):
        path = self.root / "events"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def run(self, admitted=False, code=0, unchanged=False):
        before = self.snapshot()
        event_offset = len(self.events())
        # Use the parsed manifest's bash command, with tracing enabled to prove
        # the actual wrapper disables it before reading synthetic credentials.
        proc = subprocess.Popen(["/bin/bash", "-x", str(self.checkout / self.command[1])],
                                cwd=self.checkout, env=self.env, stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, start_new_session=True)
        try:
            output, _ = proc.communicate(timeout=TIMEOUT)
        finally:
            try: os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            proc.wait(timeout=5)
        assert proc.returncode == code if admitted else proc.returncode != 0, (self.root.name, output)
        assert not any(secret in output for secret in (*CANARIES, "private-push-canary")), "credential leak"
        events = self.events()[event_offset:]
        assert not any(e["kind"] == "forbidden" for e in events), (self.root.name, events)
        calls = [e for e in events if e["kind"] == "delegate"]
        assert len(calls) == int(admitted), (self.root.name, output)
        after = self.snapshot()
        if not admitted and not self.env.get("WIZARD_FAULT", "").startswith(("remove:", "transport", "mktemp", "chmod", "mv")):
            assert not any(e["kind"] == "write" for e in events), "premutation refusal wrote resources"
        if not admitted or unchanged:
            if not self.env.get("WIZARD_FAULT", "").startswith(("remove:", "transport")) or unchanged:
                assert before == after, "refusal changed bytes, modes, types or absence"
            else:
                env_key = str(self.envfile.relative_to(self.root))
                assert {k: v for k, v in before.items() if k != env_key} == {k: v for k, v in after.items() if k != env_key}, "setter failure changed protected resources"
        if admitted:
            assert output.count("Herdr Mobile Relay stable tunnel setup") == 1
            assert calls[0]["env"] == str(self.envfile) and calls[0]["wrapped"] == "1"
            text = self.envfile.read_text()
            for key in ("HERDR_GATEWAY_URL", "HERDR_GATEWAY_SELECTION", "HERDR_TAILSCALE_ORIGIN",
                        "HERDR_EXTERNAL_HTTPS_ORIGIN", "HERDR_TAILSCALE_HTTPS_PORT", "HERDR_RELAY_RUN_ID",
                        "HERDR_RELAY_CONTROL_RUN_ID", "HERDR_RELAY_PAIRING_SOCKET", "HERDR_RELAY_REARM_BOOTSTRAP",
                        "HERDR_TAILSCALE_REQUEST"):
                assert key + "=" not in text, "persisted stale selection"
            # Only the admitted env and its newly required ancestors may change.
            allowed = {str(self.envfile.relative_to(self.root))}
            allowed.update(str(p.relative_to(self.root)) for p in self.envfile.parents if self.root in p.parents)
            assert {k: v for k, v in before.items() if k not in allowed} == {k: v for k, v in after.items() if k not in allowed}
        assert not list(self.root.rglob(".relay-env.*")), "uncommitted setter temporary"
        assert not (self.root / "evaluated").exists(), "configuration executed"
        assert not any((self.root / p).exists() for p in ("uncreated-state", "uncreated-cache", "uncreated-data"))
        return output


def wizard_cases(parent):
    assert os.getuid() != 0, "wizard fixtures require an owned non-root hosted user"
    serial = 0
    def fixture(platform, text="HERDR_RELAY_TRANSPORT=cloudflare\n"):
        nonlocal serial
        serial += 1
        return WizardFixture(parent, str(serial) + "-" + platform, platform, text)

    for platform in ("Linux", "Darwin"):
        # Native Cloudflare candidates persist the binary, config and defaults.
        # Reuse this full shape through the actual manifest wrapper, twice, while
        # preserving every non-transport field and all synthetic private state.
        for choice in ("installed", "explicit", "plugin", "caller-override", "user-local", "symlink", "parent-alias"):
            f = fixture(platform)
            binary = f.bin / "cloudflared"
            if choice == "installed":
                f.installed(f.envfile)
                f.env.pop("HERDR_RELAY_ENV")
                f.env["HERDR_PLUGIN_CONFIG_DIR"] = str(f.root / "uncreated-plugin")
                write(f.relay / ".env", "HERDR_RELAY_TRANSPORT=tailscale-cli\n")
            elif choice == "explicit":
                write(f.definition, "undecodable installed definition\n")
            elif choice == "plugin":
                f.select(f.root / "plugin/relay.env", explicit=False)
                f.env["HERDR_PLUGIN_CONFIG_DIR"] = str(f.envfile.parent)
            elif choice == "caller-override":
                binary = f.root / "tools with space/cloudflared's copy"
                binary.parent.mkdir()
                shutil.copyfile(f.bin / "cloudflared", binary); binary.chmod(0o700)
                f.env.update(CLOUDFLARED_BIN=str(binary), WIZARD_CALLER_BIN=str(binary))
            elif choice == "user-local":
                # setup.sh installs here; the pane need not already include it
                # in PATH, but stable/native consumers prepend this directory.
                binary = f.root / ".local/bin/cloudflared"
                binary.parent.mkdir(parents=True)
                shutil.copyfile(f.bin / "cloudflared", binary); binary.chmod(0o700)
            elif choice == "symlink":
                target = f.root / "native-tool"
                shutil.copyfile(binary, target); target.chmod(0o700)
                binary.unlink(); binary.symlink_to(target)
            elif choice == "parent-alias":
                alias = f.root / "linked-bin"
                alias.symlink_to(f.bin, target_is_directory=True)
                f.env["PATH"] = str(alias) + ":" + f.env["PATH"]
                binary = alias / "cloudflared"
            f.native_cloudflare(binary)
            for rerun in range(2):
                f.run(admitted=True)
                assert all(e["native_shape"] for e in f.events() if e["kind"] == "delegate")
            assert len([e for e in f.events() if e["kind"] == "delegate"]) == 2
        # A saved executable is not authority to introduce arbitrary routing,
        # even if executable. Matching an explicit caller choice still requires
        # an absolute, unambiguous, control-free regular executable spelling.
        for unsafe in ("other-route", "ambient-conflict", "empty", "relative", "missing", "directory", "fifo", "non-executable", "control", "dot", "repeated-slash"):
            f = fixture(platform)
            binary = f.root / "other-tool"
            if unsafe in ("other-route", "ambient-conflict", "non-executable"):
                shutil.copyfile(f.bin / "cloudflared", binary)
                binary.chmod(0o600 if unsafe == "non-executable" else 0o700)
            if unsafe == "ambient-conflict":
                f.env["CLOUDFLARED_BIN"] = str(binary)
                binary = f.bin / "cloudflared"
            elif unsafe == "empty": binary = ""
            elif unsafe == "relative": binary = "bin/cloudflared"
            elif unsafe == "directory": binary.mkdir()
            elif unsafe == "fifo": os.mkfifo(binary, 0o700)
            elif unsafe == "control": binary = str(binary) + "\t"
            elif unsafe == "dot": binary = str(f.bin) + "/../bin/cloudflared"
            elif unsafe == "repeated-slash": binary = str(f.bin) + "//cloudflared"
            if unsafe not in ("other-route", "ambient-conflict"):
                f.env["CLOUDFLARED_BIN"] = str(binary)
            f.native_cloudflare(binary)
            f.run()
        for plugin in (False, True):
            f = fixture(platform, None)
            selected = f.root / "uncreated-plugin/relay.env" if plugin else f.relay / ".env"
            f.select(selected, explicit=False)
            if plugin: f.env["HERDR_PLUGIN_CONFIG_DIR"] = str(selected.parent)
            f.run(admitted=True)
        f = fixture(platform, None)
        f.select(f.root / "plugin/relay.env", explicit=False)
        f.env["HERDR_PLUGIN_CONFIG_DIR"] = str(f.envfile.parent)
        write(f.envfile, "HERDR_RELAY_TRANSPORT=cloudflare\n")
        f.run(admitted=True)
        for mode in ("cloudflare", "gateway", "legacy", "literal"):
            text = "HERDR_RELAY_TRANSPORT='  ClOuDfLaRe  '\n" if mode == "cloudflare" else "HERDR_GATEWAY_URL=' wss://gateway.example.test/ '\nHERDR_GATEWAY_SELECTION=latency\n"
            if mode == "gateway": text += "HERDR_RELAY_TRANSPORT=' Gateway '\n"
            if mode == "literal": text += "  export HERDR_GATEWAY_URL=' wss://gateway.example.test/ '\nGH_TOKEN='fixture-saved-token'\n"
            f = fixture(platform, text + "HERDR_TAILSCALE_ORIGIN=https://stale.example.test\nHERDR_EXTERNAL_HTTPS_ORIGIN=https://stale.example.test\nHERDR_TAILSCALE_HTTPS_PORT=443\n")
            f.env.update(HERDR_GATEWAY_URL="wss://inherited.example.test", HERDR_GATEWAY_SELECTION="ordered",
                         HERDR_TAILSCALE_ORIGIN="https://stale.example.test", HERDR_EXTERNAL_HTTPS_ORIGIN="https://stale.example.test",
                         GH_TOKEN=CANARIES[0], GITHUB_TOKEN=CANARIES[0])
            output = f.run(admitted=True)
            if mode != "cloudflare": assert "Switching this relay from the WebRTC gateway to Cloudflare." in output
            assert [e for e in f.events() if e["kind"] == "delegate"][0]["raw_present"]
            if mode == "literal": assert "GH_TOKEN='fixture-saved-token'" in f.envfile.read_text()
        f = fixture(platform)
        with f.envfile.open("a") as out:
            out.write("HERDR_GITHUB_TOKEN_FILE='" + str(f.root / "protected/github-token") + "'\n")
        f.run(admitted=True)
        f = fixture(platform)
        with f.envfile.open("a") as out:
            out.write("HERDR_GITHUB_TOKEN_FILE='" + str(f.envfile) + "'\n")
        f.run()
        f = fixture(platform)
        with f.envfile.open("a") as out:
            out.write("GH_TOKEN='$(touch " + str(f.root / "evaluated") + ")'\n")
            out.write("  export HERDR_RELAY_TRANSPORT='cloud'\"flare\"\n")
        f.run(admitted=True)
        for false in ("", "0", "false", "False", "FALSE", "f", "F"):
            f = fixture(platform, f"HERDR_RELAY_REARM_BOOTSTRAP='{false}'\nHERDR_TAILSCALE_REQUEST='{false}'\n")
            f.env.update(HERDR_RELAY_REARM_BOOTSTRAP=false, HERDR_TAILSCALE_REQUEST=false)
            f.run(admitted=True)
        # Installed configuration beats plugin defaults; explicit selection beats
        # even an undecodable installed definition. Neither inspects legacy data.
        f = fixture(platform, "HERDR_GATEWAY_URL=wss://gateway.example.test\n")
        f.installed(f.envfile)
        f.env.pop("HERDR_RELAY_ENV")
        f.env["HERDR_PLUGIN_CONFIG_DIR"] = str(f.root / "uncreated-plugin")
        write(f.relay / ".env", "HERDR_RELAY_TRANSPORT=tailscale-cli\n")
        f.run(admitted=True)
        f = fixture(platform)
        write(f.definition, "undecodable installed definition\n")
        f.run(admitted=True)
        for fault in ("malformed", "absent-env", "unreadable", "directory", "symlink", "read-failure"):
            f = fixture(platform)
            f.installed(f.envfile)
            f.env.pop("HERDR_RELAY_ENV")
            if fault == "malformed": write(f.definition, "undecodable\n")
            if fault == "absent-env": f.installed(f.root / "missing/relay.env")
            if fault == "unreadable": f.definition.chmod(0o200)
            if fault == "directory": f.definition.unlink(); f.definition.mkdir()
            if fault == "symlink": f.definition.unlink(); f.definition.symlink_to(f.root / "missing-definition")
            if fault == "read-failure": f.env.update(WIZARD_FAULT="head", WIZARD_READ_TARGET=str(f.definition))
            f.run()
        for mode in ("cloudflare", "tailscale-cli", "tailscale", "tailscale-external"):
            f = fixture(platform, None)
            write(f.relay / ".env", "HERDR_RELAY_TRANSPORT=" + mode + "\n")
            f.select(f.root / "uncreated-plugin/relay.env", explicit=False)
            f.env["HERDR_PLUGIN_CONFIG_DIR"] = str(f.envfile.parent)
            f.run()
        f = fixture(platform)
        write(f.relay / ".env", "HERDR_GATEWAY_URL=wss://gateway.example.test\n")
        f.select(f.relay / ".env")
        f.env["HERDR_PLUGIN_CONFIG_DIR"] = str(f.root / "uncreated-plugin")
        f.run(admitted=True)
        for spelling in ("relative/relay.env", str(parent.resolve()) + "/uncreated/../relay.env",
                         str(parent.resolve()) + "//relay.env", str(parent.resolve()) + "/./relay.env",
                         str(parent.resolve()) + "/relay.env\n"):
            f = fixture(platform)
            f.env["HERDR_RELAY_ENV"] = spelling
            f.run()
        f = fixture(platform)
        (f.root / "alias-parent").symlink_to(f.envfile.parent, target_is_directory=True)
        f.env["HERDR_RELAY_ENV"] = str(f.root / "alias-parent/relay.env")
        f.run()
        for key, values in (
                ("HERDR_RELAY_TRANSPORT", ("tailscale-cli", "tailscale", "tailscale-external", "unknown", "cloud flare", "tail scale-cli", " TAILSCALE-CLI ")),
                ("HERDR_RELAY_RUN_ID", ("foreground", " ")),
                ("HERDR_RELAY_CONTROL_RUN_ID", ("foreground",)),
                ("HERDR_RELAY_PAIRING_SOCKET", ("/fixture/control",)),
                ("HERDR_RELAY_REARM_BOOTSTRAP", ("1", "true", "unknown", "no", "off", " false ")),
                ("HERDR_TAILSCALE_REQUEST", ("1", "unknown")),
                ("HERDR_GATEWAY_SELECTION", ("unknown", "lat ency")),
                ("HERDR_GATEWAY_URL", ("wss://gate way.example.test", "https://gateway.example.test", "wss://user:private@gateway.example.test"))):
            for value in values:
                for ambient in (False, True):
                    f = fixture(platform, f"{key}=''\n" if ambient else f"{key}='{value}'\n")
                    if ambient: f.env[key] = value
                    f.env["HERDR_TAILSCALE_CLI_ALLOW_UNREGISTERED_START"] = "1"
                    f.env["HERDR_TAILSCALE_CLI_SCOPE"] = "development"
                    f.env.update(GH_TOKEN=CANARIES[0], GITHUB_TOKEN=CANARIES[0])
                    f.run()
        for saved in ("cloudflare", "gateway"):
            for ambient in ("tailscale-cli", "tailscale", "tailscale-external"):
                f = fixture(platform, f"HERDR_RELAY_TRANSPORT={saved}\nHERDR_GATEWAY_URL=wss://gateway.example.test\n")
                f.env["HERDR_RELAY_TRANSPORT"] = ambient
                f.run()
        for session in ("tailscale-session.env", "tailscale-external-session.env"):
            for dangling in (False, True):
                f = fixture(platform)
                path = f.envfile.parent / session
                if dangling: path.symlink_to(f.root / "missing-session")
                else: write(path, "synthetic-session\n")
                f.run()
        # Saved release roots can route real stable-state helpers after load.
        # Refuse in the parent, before gateway edits, even if the helper exists
        # or the caller happens to name the same root. The inert child is not
        # proof about the real delegate: no invocation may reach it here.
        for selection in ("explicit", "installed", "plugin"):
            for present in (False, True):
                f = fixture(platform)
                release = f.root / "unadmitted-release"
                if present:
                    helper = release / "current/herdr-mobile-relay"
                    helper.parent.mkdir(parents=True)
                    shutil.copyfile(f.bin / "herdr-mobile-relay", helper); helper.chmod(0o700)
                    f.env["HERDR_RELEASE_ROOT"] = str(release)
                if selection == "installed":
                    f.installed(f.envfile); f.env.pop("HERDR_RELAY_ENV")
                    f.env["HERDR_PLUGIN_CONFIG_DIR"] = str(f.root / "uncreated-plugin")
                elif selection == "plugin":
                    f.select(f.root / "plugin/relay.env", explicit=False)
                    f.env["HERDR_PLUGIN_CONFIG_DIR"] = str(f.envfile.parent)
                write(f.relay / ".env", "HERDR_RELAY_TRANSPORT=tailscale-cli\n")
                write(f.envfile, "HERDR_RELAY_TRANSPORT=gateway\nHERDR_GATEWAY_URL='wss://gateway.example.test'\n"
                      "HERDR_GATEWAY_SELECTION=latency\nGH_TOKEN='" + CANARIES[1] + "'\n"
                      "  export HERDR_RELEASE_ROOT='" + str(release) + "'\n"
                      '\tHERDR_RELEASE_ROOT="' + str(release) + '"\n')
                f.env["GH_TOKEN"] = CANARIES[0]
                output = f.run()
                assert "no setup was started" in output
                assert "Switching this relay" not in output and "stable tunnel setup" not in output
                assert not release.exists() if not present else helper.is_file()
        # Wildcard audit: other executable/interpreter/verified-web selectors
        # and stable/setup controls are caller inputs, never saved authority.
        for key in ("HERDR_WEB_ROOT", "HERDR_APP_DEPLOY_NPX", "HERDR_APP_DEPLOY_NODE_DIR",
                    "HERDR_STABLE_STATE_FILE", "HERDR_STABLE_YES", "HERDR_STABLE_REUSE_CONFIG",
                    "HERDR_STABLE_RELOGIN", "HERDR_STABLE_POLL_DELAY", "HERDR_SETUP_YES",
                    "HERDR_RELEASE_ROOT"):
            for nonempty in (False, True):
                f = fixture(platform)
                value = ("1" if key.endswith(("YES", "REUSE_CONFIG", "RELOGIN", "POLL_DELAY"))
                         else str(f.root / "unadmitted-route")) if nonempty else ""
                write(f.envfile, "HERDR_GATEWAY_URL='wss://gateway.example.test'\n"
                      "HERDR_GATEWAY_SELECTION=ordered\n  export " + key + "='" + value + "'\n")
                f.env[key] = value  # Agreement is deliberately not an exception.
                output = f.run()
                assert "no setup was started" in output
                assert "Switching this relay" not in output and "stable tunnel setup" not in output
        for text in (
                "HERDR_RELAY_TRANSPORT=cloudflare\nHERDR_RELAY_TRANSPORT=gateway\n",
                "GH_TOKEN=fixture-saved-token\nGH_TOKEN=different-saved-token\n",
                "PATH=/untrusted\n", "BASH_ENV=/untrusted\n", "ENV_FILE=/untrusted\n",
                "CURRENT_TRANSPORT=cloudflare\n", "LITERAL_VALUE=cloudflare\n", "NATIVE_SELECTED_POINTER=/untrusted\n",
                "HERDR_RELAY_BIN=/untrusted\n", "HERDR_PLUTIL=/untrusted\n", "CLOUDFLARED_BIN=/untrusted\n",
                "HERDR_RELAY_ENV=/untrusted\n", "HERDR_PLUGIN_CONFIG_DIR=/untrusted\n",
                "HERDR_RELAY_TRANSPORT=cloudflare; false\n", "GH_TOKEN=\"$SECRET\"\n",
                "HERDR_RELAY_TRANSPORT='unterminated\n", "HERDR_RELAY_TRANSPORT=cloudflare\r\n",
                "#" * 8193 + "\n", "#\n" * 4097, "#" * 262145):
            f = fixture(platform, text)
            f.run()
        for code_shape in ("HERDR_RELAY_TRANSPORT=$(touch '{marker}')\n", "touch '{marker}'\n", "GH_TOKEN=`touch '{marker}'`\n"):
            f = fixture(platform)
            write(f.envfile, code_shape.format(marker=f.root / "evaluated"))
            f.run()
        for unsafe in ("symlink", "dangling", "directory", "fifo", "hardlink", "unreadable", "writable"):
            f = fixture(platform)
            if unsafe in ("symlink", "dangling", "directory", "fifo"):
                f.envfile.unlink()
                if unsafe == "directory": f.envfile.mkdir()
                elif unsafe == "fifo": os.mkfifo(f.envfile, 0o600)
                else: f.envfile.symlink_to(f.root / ("protected/github-token" if unsafe == "symlink" else "missing-env"))
            if unsafe == "hardlink": os.link(f.envfile, f.root / "alias-env")
            if unsafe == "unreadable": f.envfile.chmod(0o200)
            if unsafe == "writable": f.envfile.chmod(0o666)
            f.run()
        f = fixture(platform)
        f.envfile.write_bytes(b"HERDR_RELAY_TRANSPORT=cloud\x00flare\n")
        f.run()
        for fault in ("head", "wc", "stat"):
            f = fixture(platform, "GH_TOKEN=fixture-saved-token\n")
            f.env["WIZARD_FAULT"] = fault
            f.run()
        for fault in ("mktemp", "chmod", "mv", "transport"):
            f = fixture(platform)
            f.env["WIZARD_FAULT"] = fault
            f.run(unchanged=True)
        for key in ("HERDR_GATEWAY_URL", "HERDR_GATEWAY_SELECTION", "HERDR_TAILSCALE_ORIGIN", "HERDR_TAILSCALE_HTTPS_PORT", "HERDR_EXTERNAL_HTTPS_ORIGIN", "HERDR_RELAY_REARM_BOOTSTRAP", "HERDR_TAILSCALE_REQUEST"):
            text = "HERDR_GATEWAY_URL=wss://gateway.example.test\nHERDR_GATEWAY_SELECTION=latency\nHERDR_TAILSCALE_ORIGIN=https://stale.example.test\nHERDR_TAILSCALE_HTTPS_PORT=443\nHERDR_EXTERNAL_HTTPS_ORIGIN=https://stale.example.test\nHERDR_RELAY_REARM_BOOTSTRAP=false\nHERDR_TAILSCALE_REQUEST=0\n"
            f = fixture(platform, text)
            f.env["WIZARD_FAULT"] = "remove:" + key
            output = f.run(unchanged=key == "HERDR_GATEWAY_URL")
            assert "selection could not be persisted" in output
        for code in (23, 130, 143):
            f = fixture(platform)
            f.env["WIZARD_CHILD_CODE"] = str(code)
            assert "Stable setup did not complete" in f.run(admitted=True, code=code)
        for sig in ("SIGINT", "SIGTERM"):
            f = fixture(platform)
            f.env["WIZARD_CHILD_SIGNAL"] = sig
            f.run(admitted=True, code=128 + getattr(signal, sig))


def main():
    with tempfile.TemporaryDirectory(prefix="herdr-service-dispatch-") as tmp:
        parent = pathlib.Path(tmp)
        wizard_cases(parent)
        unavailable_cases(parent)
        # A separate inert process proves supervisor cleanup is not global.
        stranger = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(120)"], start_new_session=True)
        try:
            for mode in ("gateway", "legacy"):
                for child_code in (0, 23):
                    fixture = Fixture(parent, "wrapper-" + mode + "-" + str(child_code), mode)
                    fixture.env["CLOUDFLARED_BIN"] = str(fixture.root / "absent-cloudflared")
                    fixture.env["CLOUDFLARED_CONFIG"] = str(fixture.root / "absent-config")
                    code, started = fixture.wrapper(code=child_code)
                    assert code == (child_code or 1) and [e["kind"] for e in started] == ["relay"]
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
            for mode in ("gateway", "cloudflare"):
                for sig in (signal.SIGTERM, signal.SIGINT):
                    fixture = Fixture(parent, "signal-" + mode + "-" + str(sig), mode)
                    code, started = fixture.wrapper(termination=sig)
                    assert code == 128 + sig and len(started) == (2 if mode == "cloudflare" else 1)
            for child in ("relay", "tunnel"):
                for child_code in (0, 23):
                    fixture = Fixture(parent, "cloudflare-exit-" + child + "-" + str(child_code), "cloudflare")
                    code, started = fixture.wrapper(child, child_code)
                    assert code == (child_code or 1) and sorted(e["kind"] for e in started) == ["relay", "tunnel"]
            for missing in ("binary", "config"):
                fixture = Fixture(parent, "cloudflare-missing-" + missing, "cloudflare")
                fixture.env["CLOUDFLARED_BIN" if missing == "binary" else "CLOUDFLARED_CONFIG"] = str(fixture.root / "absent")
                code, started = fixture.wrapper()
                assert code == 78 and not started
            # Reserved child failures are NOT launchd admission refusals.
            for launchd in (False, True):
                for mode in ("gateway", "cloudflare"):
                    for child in (("relay", "tunnel") if mode == "cloudflare" else ("relay",)):
                        for child_code in (0, 23, 69, 78):
                            fixture = Fixture(parent, f"adapter-child-{launchd}-{mode}-{child}-{child_code}", mode)
                            code, started = fixture.wrapper(child, child_code, launchd=launchd)
                            assert code == (child_code or 1) and len(started) == (2 if mode == "cloudflare" else 1)
                    for sig in (signal.SIGINT, signal.SIGTERM):
                        fixture = Fixture(parent, f"adapter-signal-{launchd}-{mode}-{sig}", mode)
                        code, started = fixture.wrapper(termination=sig, launchd=launchd)
                        assert code == 128 + sig and len(started) == (2 if mode == "cloudflare" else 1)
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
                    assert not started and code == (69 if mode in ("tailscale-cli", "cli-staged") else 78)
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
                        if failure != "readiness":
                            assert (fixture.root / "activation").read_bytes() == activation
                        else:
                            # FAILURE=readiness also makes the PRIOR /readyz unready, so no readiness
                            # evidence exists for the previously active service. Rollback must restore
                            # the files but must not reactivate an unverified service: it stays
                            # quiesced and recovery evidence is retained (A1-F007 transaction rollback).
                            assert "rollback is incomplete" in result.stderr
                            assert list((fixture.root / "state").glob("**/herdr-native-recovery.*"))
                            assert not any(s["active"] for s in json.loads((fixture.root / "activation").read_text()).values())
                    assert (fixture.root / "config/phone-app-origin-configured").read_text() == "https://app.example.test\n"
                    assert (fixture.root / "config/tunnel.yml").read_text() == "hostname: relay.example.test\n"
        finally:
            stranger.terminate()
            stranger.wait(timeout=5)
    print("hosted service transport dispatch fixtures passed")


if __name__ == "__main__":
    main()

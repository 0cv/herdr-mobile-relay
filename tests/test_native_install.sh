#!/usr/bin/env bash
set -euo pipefail
# Guard before temporary roots, subprocesses and builds. Never fabricate flags.
if [ "${GITHUB_ACTIONS:-}" != true ] || [ "${RUNNER_ENVIRONMENT:-}" != github-hosted ] || [ "$EUID" -eq 0 ]; then
    echo "native installation fixtures require disposable GitHub-hosted CI" >&2
    exit 2
fi
python3 -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 11) else 2)' || exit 2
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-native-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT
export NATIVE_HELPER="$WORK_DIR/helper"
python3 - "$NATIVE_HELPER" "$REPO_DIR/cmd/herdr-mobile-relay" <<'PY'
import subprocess, sys
subprocess.run(["go", "build", "-o", sys.argv[1], sys.argv[2]], check=True, timeout=180)
PY
# Existing production-dispatch, diagnostic and navigation suites remain wired;
# they retain their separate acceptance boundaries and their own hosted guards.
# The service qualification harness sets HERDR_NATIVE_INSTALL_SKIP_NESTED_DISPATCH=1 for this
# suite only, because it runs the dispatch suite as its own qualification suite (its own exit
# status and timing; run twice, the nested copy used most of this suite's wall-clock bound and
# the native cells below could not finish). Without the variable (the check.yml shell job, a
# developer run) the nested execution is unchanged.
python3 - "$REPO_DIR" <<'PY'
import os, pathlib, subprocess, sys
repo = pathlib.Path(sys.argv[1])
for name, timeout in (("test_service_transport_dispatch.py", 900),
                      ("test_plugin_doctor.py", 120), ("test_setup_menu.py", 180)):
    if name == "test_service_transport_dispatch.py" and os.environ.get("HERDR_NATIVE_INSTALL_SKIP_NESTED_DISPATCH") == "1":
        print("test_service_transport_dispatch.py runs as its own qualification suite; not nested here", flush=True)
        continue
    subprocess.run([sys.executable, str(repo / "tests" / name)], check=True, timeout=timeout)
PY

# Reuse the actual-copy fixture, strict compiled helper and dynamic, inert
# label-specific controllers. No duplicated/hand-copied production policy.
python3 - "$REPO_DIR" "$WORK_DIR" <<'PY'
import importlib.util, json, os, pathlib, signal, stat, subprocess, sys, time
repo, parent = map(pathlib.Path, sys.argv[1:])
spec = importlib.util.spec_from_file_location("dispatch_fixture", repo / "tests/test_service_transport_dispatch.py")
d = importlib.util.module_from_spec(spec)
spec.loader.exec_module(d)
canary = "fixture-native-selected-secret"


def snapshot(paths):
    return {str(p): (p.read_bytes(), stat.S_IMODE(p.stat().st_mode)) if p.exists() else None for p in paths}


def resources(f):
    return [f.definition, f.legacy, f.envfile, f.root / "config/github-token"]


def recovery(f):
    return list((f.root / "state").glob("**/herdr-native-recovery.*"))


def state(f):
    return json.loads((f.root / "activation").read_text())


NATIVE_RUNS = [0]


def run(f, conditional=""):
    # Ordinary cells use the REAL installer. Conditional cells call the REAL
    # transaction API with a conditional EXIT trap, not a conditional source
    # of the whole installer (which would mask unrelated public-gate errexit).
    started = time.monotonic()
    command = ["/bin/bash", "-x", str(f.script)]
    if conditional:
        command = ["/bin/bash", "-x", "-c", NATIVE_EXIT_PROBE, str(f.script), str(f.script.parent), f.platform,
                   str(f.definition), str(f.legacy), conditional]
    proc = subprocess.Popen(command, env=f.env, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
    try:
        stdout, stderr = proc.communicate(timeout=40)
        result = subprocess.CompletedProcess(command, proc.returncode, stdout, stderr)
        f.check_safe(stdout + stderr)
        assert canary not in stdout + stderr, "native credential exposed"
        return result
    finally:
        # Progress evidence only: a suite that reaches its wall-clock bound otherwise
        # leaves no trace of how many cells ran or how long each took.
        NATIVE_RUNS[0] += 1
        print("native installation fixture invocation %d (%s) took %.1fs" % (
            NATIVE_RUNS[0], f.platform, time.monotonic() - started), file=sys.stderr, flush=True)
        # This fixture invocation group only, never native cleanup evidence.
        try: os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError: pass
        proc.wait(timeout=5)


# F006: actual transaction-local acquisition and actual native_install_exit.
# This harness sources exact copied originals, never substitutes observers.
# Ordinary full-installer cells use actual generated definitions; this API
# control uses an inert candidate marker, not native-definition qualification.
NATIVE_EXIT_PROBE = r'''
set +x
set -euo pipefail
SCRIPT_DIR="$1"
. "$1/common.sh"
. "$1/native-install-transaction.sh"
ENV_FILE="$HERDR_RELAY_ENV"
if [ "$2" = systemd ]; then
    label=herdr-mobile-relay.service; legacy=herdr-remote.service
else
    label=com.herdr-mobile-relay.service; legacy=com.herdr-remote.service
fi
native_installer_admit "$ENV_FILE" "$ENV_FILE" "$3" "$4" || exit 1
native_install_begin "$2" "$3" "$4" "$ENV_FILE" "$label" "$legacy" "$NATIVE_SIDECAR" || exit 1
case "$5" in
    if) trap 'if native_install_exit; then :; fi' EXIT ;;
    or) trap 'native_install_exit || exit 1' EXIT ;;
    *) exit 97 ;;
esac
native_installer_candidate "$ENV_FILE" "${CLOUDFLARED_CONFIG:-}" || exit 1
printf 'inert transaction replacement marker\n' > "$native_recovery/new.definition" || exit 1
chmod 600 "$native_recovery/new.definition" || exit 1
native_install_publish "$native_recovery/new.definition" || exit 1
native_install_activate || exit 1
instance="$(env_file_value "$ENV_FILE" HERDR_RELAY_INSTANCE_ID)" || exit 1
health="$(wait_for_relay_health 18375 15 1 "$instance")" || exit 23
if [ "$TRANSPORT" = gateway ]; then
    origin="$(gateway_install_phone_app_origin "$ENV_FILE")" || exit 1
    verify_gateway_install_readiness 18375 "$health" "$origin" || exit 23
else
    verify_public_readiness "$ENV_FILE" "$health" || exit 23
fi
exit 97  # A fault cell must never pass its actual readiness/public gate.
'''

NATIVE_PROBE = r'''
set +x
set -euo pipefail
. "$1/common.sh"
. "$1/native-install-transaction.sh"
native_manager="$2"
if [ "$4" = reader ]; then native_install_read_bounded "$5"; exit $?; fi
result=0
case "$4" in
    observe-if) if native_install_observe "$3"; then :; else result=1; fi ;;
    observe-or) native_install_observe "$3" || result=1 ;;
    quiet-if) if native_install_label_quiet "$3"; then :; else result=1; fi ;;
    quiet-or) native_install_label_quiet "$3" || result=1 ;;
    *) exit 97 ;;
esac
if [ "$5" = unknown ] && [ "$result" -ne 0 ]; then
    [ "$NATIVE_STATE:$NATIVE_ENABLED" = unknown:unknown ] || exit 97
fi
printf '%s:%s\n' "$NATIVE_STATE" "$NATIVE_ENABLED"
exit "$result"
'''


def probe(f, label, invocation, expectation="unknown", data=None, limit=4096):
    argument = str(limit) if invocation == "reader" else expectation
    command = ["/bin/bash", "-c", NATIVE_PROBE, "native-probe", str(f.script.parent), f.platform, label, invocation, argument]
    proc = subprocess.Popen(command, env=f.env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, start_new_session=True)
    try:
        stdout, stderr = proc.communicate(input=data, timeout=20)
        f.check_safe((stdout + stderr).decode())
        return proc.returncode, stdout
    finally:
        try: os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError: pass
        proc.wait(timeout=5)


def capture_control(f, seam, role, payload, status=0, when="initial", at=1):
    d.write(f.root / "native-capture.json", json.dumps(dict(seam=seam, label=role, hex=payload.hex(), status=status, when=when, at=at)))


def capture_frames(platform, role):
    label = ("herdr-mobile-relay.service" if role == "current" else "herdr-remote.service") if platform == "systemd" else (
        "com.herdr-mobile-relay.service" if role == "current" else "com.herdr-remote.service")
    if platform == "systemd":
        return label, [("systemd", b"LoadState=loaded\nActiveState=inactive\nUnitFileState=disabled\n", 0, b"inactive", 4096)]
    negative = ('Could not find service "' + label + '" in domain for user gui: ' + str(os.getuid())).encode()
    disabled = ('disabled services = {\n "' + label + '" => true\n}').encode()
    return label, [("domain", b"gui/fixture = {}", 0, b"fixture", 65536),
                   ("label", negative, 113, b"service", 65536),
                   ("disabled", disabled, 0, b"true", 65536)]


def capture_faults(seam, payload, status, part, limit):
    return [("middle-nul", payload.replace(part, part[:2] + b"\x00" + part[2:], 1), status),
            ("prefix-nul", b"\x00" + payload, status), ("suffix-nul", payload + b"\x00", status),
            ("oversized", b"x" * (limit + 1), status),
            # An exact nonzero launchd absence is SUPPORTED, not a failed
            # capture. Failed label probes must have a nonmatching diagnostic.
            ("failed", b"controller unavailable" if seam == "label" else payload, 2),
            ("partial", payload[:len(payload) // 2], 2), ("empty", b"", 2)]


def native_capture_cases(parent, platform, transport):
    for role in ("current", "legacy"):
        label, frames = capture_frames(platform, role)
        for seam, payload, status, part, limit in frames:
            faults = capture_faults(seam, payload, status, part, limit)
            for name, data, code in faults:
                for invocation in ("observe-if", "observe-or", "quiet-if", "quiet-or"):
                    f = d.Fixture(parent, f"capture-probe-{platform}-{transport}-{role}-{seam}-{name}-{invocation}", transport, platform)
                    before = snapshot(resources(f)); capture_control(f, seam, role, data, code)
                    result, output = probe(f, label, invocation)
                    assert result == 1 and output == b"unknown:unknown\n"
                    assert snapshot(resources(f)) == before and not recovery(f)
                    assert not any(e["kind"] == "native-live-attempt" for e in f.events())
                    assert bystander.poll() is None
            # Initial discovery, post-snapshot/polling, then ACTUAL readiness
            # and public failure EXIT rollback after replacement activation.
            for initial in (False, True):
                cells = [("initial", 1, name, data, code, "") for name, data, code in faults]
                cells += [("fresh", number, "middle-nul", faults[0][1], status, "") for number in (2, 3, 4)]
                cells += [(phase, 1, name, data, code, conditional)
                          for phase in ("ready", "public")
                          for name, data, code in faults[:6]
                          for conditional in ("", "if", "or")]
                for phase, at, name, data, code, conditional in cells:
                    f = d.Fixture(parent, f"capture-exit-{platform}-{transport}-{role}-{seam}-{initial}-{phase}-{at}-{name}-{conditional}", transport, platform, initial)
                    d.write(f.root / "config/github-token", "prior-unselected-sidecar\n")
                    (f.root / "config/github-token").chmod(0o640)
                    with f.envfile.open("a") as out: out.write("GH_TOKEN='" + canary + "'\n")
                    before = snapshot(resources(f)); protected = snapshot([f.root / "config/devices.json", f.root / "config/tunnel.yml"])
                    # Global domain/map counters count both begin observations.
                    if seam in ("domain", "disabled"):
                        at = (2 if role == "legacy" else 1) if phase == "initial" else at + 1 if phase == "fresh" else 1
                    capture_control(f, seam, role, data, code, "rollback" if phase in ("ready", "public") else "initial", at)
                    if phase in ("ready", "public"):
                        f.env["NATIVE_FAILURE"] = phase + "-capture"
                        if phase == "public": f.env["FAILURE"] = "app"
                    result = run(f, conditional)
                    assert result.returncode != 0
                    events = f.events(); fault = next(i for i, e in enumerate(events) if e["kind"] == "native-capture-fault")
                    assert not any(e["kind"] == "native-live-attempt" for e in events[fault + 1:]), "destructive attempt after unknown observation"
                    assert not any(e["kind"] == "manager" and e["command"] in ("restart", "bootstrap", "kickstart", "enable") for e in events[fault + 1:]), "prior activation after unknown observation"
                    frozen = json.loads((f.root / "native-fault-snapshot.json").read_text())
                    for path, value in frozen.items():
                        p = f.root / path
                        assert ([p.read_bytes().hex(), stat.S_IMODE(p.stat().st_mode)] if p.exists() else None) == value
                    assert snapshot(map(pathlib.Path, protected)) == protected and bystander.poll() is None
                    if phase in ("ready", "public"):
                        assert (f.root / "replacement-active").exists() and (f.root / "native-capture-armed").exists()
                        armed = json.loads((f.root / "native-armed-snapshot.json").read_text())
                        assert armed == frozen, "rollback mutated replacement resources before refusal"
                        assert f.definition.read_text() != "previous definition\n"
                        assert (f.root / "config/github-token").read_text() == canary + "\n"
                        assert recovery(f) and "rollback is incomplete" in result.stderr
                        if role == "current" or seam in ("domain", "disabled"):
                            assert state(f)["current"]["active"], "active replacement must remain untouched on first unknown probe"
                    else:
                        assert snapshot(resources(f)) == before
                        if phase == "fresh": assert recovery(f) and "rollback is incomplete" in result.stderr
                        else: assert not recovery(f)  # begin never completed; no publication
                    for directory in recovery(f): assert stat.S_IMODE(directory.stat().st_mode) == 0o700
                    if phase != "initial":
                        saved = json.loads((f.root / "native-fault-recovery.json").read_text())
                        assert saved
                        for path, value in saved.items():
                            p = f.root / path
                            assert p.exists() and not p.is_symlink()
                            assert [p.read_bytes().hex() if p.is_file() else None, stat.S_IMODE(p.stat().st_mode)] == value
        # Supported absent/inactive quiet EOF, exact nonzero absence, loaded
        # header EOF (not quiet), and actual reader byte-bound controls.
        f = d.Fixture(parent, f"capture-valid-{platform}-{transport}-{role}", transport, platform)
        result, output = probe(f, label, "quiet-if", "supported")
        assert result == 0 and output in (b"absent:false\n", b"inactive:false\n")
        for seam, payload, code, part, limit in frames:
            capture_control(f, seam, role, payload.rstrip(b"\n"), code)
            result, output = probe(f, label, "quiet-or", "supported")
            assert result == 0 and output in (b"absent:false\n", b"inactive:false\n")
        if platform == "launchd":
            capture_control(f, "label", role, ("gui/" + str(os.getuid()) + "/" + label + " = {}").encode())
            assert probe(f, label, "observe-if", "supported") == (0, b"loaded:false\n")
            assert probe(f, label, "quiet-or", "supported") == (1, b"loaded:false\n")
        for bound in (4096, 65536):
            for data, expected in ((b"", 0), (b"x" * (bound - 1), 0), (b"x" * bound, 0),
                                   (b"x" * (bound + 1), 1), (b"x\x00", 1), (b"\x00x", 1)):
                result, output = probe(f, label, "reader", data=data, limit=bound)
                assert result == expected and output == (data if expected == 0 else b"")


def native_origin_cases(parent, platform):
    # F001: a loaded same-label service whose definition origin is NOT Herdr's own expected
    # path must be refused by native_install_begin BEFORE any quiesce, stop, disable, bootout
    # or publication, for both labels and both managers. Gateway transport only: every cell
    # runs a real installer and this suite shares one 900 s bound.
    mutating = ("stop", "disable", "enable", "restart", "bootout", "bootstrap", "kickstart", "daemon-reload")
    kinds = ["foreign-bare", "foreign-file", "empty", "duplicate"] + (
        ["dropin", "fail", "oversized", "blank"] if platform == "systemd" else ["malformed", "malformed-space", "malformed-tab"])

    def label_of(role):
        return ("herdr-mobile-relay.service" if role == "current" else "herdr-remote.service") if platform == "systemd" else (
            "com.herdr-mobile-relay.service" if role == "current" else "com.herdr-remote.service")

    def origin_queries(f):
        # systemd: the additive FragmentPath/DropInPaths query; launchd: the label print whose body
        # carries the job-level path line (there is no second query on launchd).
        if platform == "systemd":
            return [e["argv"][2] for e in f.events() if e["kind"] == "manager" and e["command"] == "show"
                    and "--property=FragmentPath" in e["argv"]]
        return [e["argv"][1].rsplit("/", 1)[1] for e in f.events() if e["kind"] == "manager" and e["command"] == "print"
                and e["argv"][1] != "gui/" + str(os.getuid())]

    for role in ("current", "legacy"):
        label = label_of(role)
        refusal = ("Native installation refused: the loaded service for " + label + " was not established to be defined by "
                   "Herdr's own definition path; no service or definition was changed.")
        for kind in kinds:
            f = d.Fixture(parent, f"origin-{platform}-{role}-{kind}", "gateway", platform, kind != "foreign-bare")
            if kind == "foreign-bare":
                # No Herdr definition exists anywhere: only a running, enabled foreign service
                # holds the label, so a missing expected definition must stay missing.
                d.write(f.root / "activation", json.dumps({k: dict(active=k == role, enabled=k == role) for k in ("current", "legacy")}))
            f.env["NATIVE_ORIGIN"] = role + ":" + ("foreign" if kind.startswith("foreign") else kind)
            before = snapshot(resources(f)); activation = state(f)
            result = run(f)
            events = f.events()
            where = platform + " " + role + " " + kind
            assert result.returncode != 0, where + ": a foreign or unverifiable origin was accepted"
            assert refusal in result.stderr, where + ": the fixed origin refusal was not printed"
            # The refusal is the SINGLE diagnostic: only the fixture's own leading `bash -x` trace
            # of the installer's first `set +x` may accompany it (no second generic failure line).
            lines = result.stderr.splitlines()
            if lines and lines[0] == "+ set +x": lines = lines[1:]
            assert lines == [refusal], where + ": stderr was not exactly the one fixed refusal line: " + repr(result.stderr)
            assert not any(e["kind"] == "manager" and e["command"] in mutating for e in events), where + ": the same-label service was acted on"
            assert not any(e["kind"] == "filesystem" and e["live"] and e["tool"] in ("cp", "mv", "rm", "chmod") for e in events), where + ": a live resource was written"
            assert snapshot(resources(f)) == before, where + ": a resource changed"
            assert state(f) == activation, where + ": the manager state changed"
            assert not recovery(f), where + ": a recovery directory remained after a refusal before any mutation"
            assert not any(e["kind"] in ("http", "app", "dispatch", "relay", "tunnel", "cli-gate") for e in events), where
            assert label in origin_queries(f), where + ": the origin evidence was never requested"
            assert bystander.poll() is None
    # Positive controls: matching origin proceeds (and the evidence is requested for BOTH labels);
    # a fresh install has no loaded label, so it issues no systemd origin query at all.
    f = d.Fixture(parent, f"origin-{platform}-match", "gateway", platform, True)
    result = run(f)
    assert result.returncode == 0 and "Native installation refused" not in result.stderr
    assert set(origin_queries(f)) == {label_of("current"), label_of("legacy")}
    f = d.Fixture(parent, f"origin-{platform}-fresh", "gateway", platform)
    result = run(f)
    assert result.returncode == 0 and "Native installation refused" not in result.stderr
    if platform == "systemd": assert not origin_queries(f)
    native_origin_input_cases(parent, platform)


def native_origin_input_cases(parent, platform):
    # The adapter's NATIVE_ORIGIN input is a closed list. A variable that is PRESENT but
    # malformed, including the empty string, must be a `forbidden` event and exit 99; it must
    # never be treated as unset, or a malformed fault-injection cell would silently stop
    # exercising its fault. Unset and every valid value must not be forbidden. The adapter is
    # driven directly in its own fixture, because run() fails on any forbidden event.
    tool, argv = ("systemctl", ["--user", "show-environment"]) if platform == "systemd" else (
        "launchctl", ["print-disabled", "gui/" + str(os.getuid())])
    systemd_only = ["dropin", "fail", "oversized", "blank"]
    launchd_only = ["malformed", "malformed-space", "malformed-tab"]
    kinds = ["foreign", "empty", "duplicate"] + (systemd_only if platform == "systemd" else launchd_only)
    valid = [role + ":" + kind for role in ("current", "legacy") for kind in kinds]
    invalid = ["", " ", "foreign", "current", "current:", ":foreign", "current:bogus", "other:foreign",
               "current:foreign:extra", "CURRENT:foreign", "current: foreign", "current:Foreign"]
    invalid += [role + ":" + kind for role in ("current", "legacy")
                for kind in (launchd_only if platform == "systemd" else systemd_only)]
    f = d.Fixture(parent, f"origin-input-{platform}", "gateway", platform)

    def invoke(value):
        env = dict(f.env)
        if value is not None: env["NATIVE_ORIGIN"] = value
        before = sum(e["kind"] == "forbidden" for e in f.events())
        proc = subprocess.run([str(f.bin / tool), *argv], env=env, stdin=subprocess.DEVNULL,
                              capture_output=True, timeout=20)
        return proc.returncode, sum(e["kind"] == "forbidden" for e in f.events()) - before

    assert invoke(None) == (0, 0), platform + ": an unset NATIVE_ORIGIN must leave the adapter unchanged"
    for value in valid:
        assert invoke(value) == (0, 0), platform + ": a valid NATIVE_ORIGIN was forbidden: " + repr(value)
    for value in invalid:
        assert invoke(value) == (99, 1), platform + ": a malformed NATIVE_ORIGIN was not forbidden: " + repr(value)


bystander = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(3600)"], start_new_session=True)
try:
    for platform in ("systemd", "launchd"):
        for transport in ("gateway", "cloudflare"):
            native_capture_cases(parent, platform, transport)
            if transport == "gateway": native_origin_cases(parent, platform)
            # Deterministic policy refusals precede all managers, HTTP/app,
            # relay/tunnel dispatch and credential/configuration mutation.
            for name, addition in (
                ("run", "HERDR_RELAY_RUN_ID=foreground\n"),
                ("control", "HERDR_RELAY_CONTROL_RUN_ID=foreground\n"),
                ("pairing", "HERDR_RELAY_PAIRING_SOCKET=/fixture/control\n"),
                ("rearm", "HERDR_RELAY_REARM_BOOTSTRAP=1\n"),
                ("rearm-invalid", "HERDR_RELAY_REARM_BOOTSTRAP=invalid\n"),
                ("conflict", "HERDR_RELAY_TOKEN=distinct\n"),
                ("expansion", "GH_TOKEN=$(herdr forbidden)\n"),
                ("routing", "PATH=/forbidden\n"),
                ("raw-distinct", "GH_TOKEN=one\nGITHUB_TOKEN=two\n"),
                ("pointer-relative", "HERDR_GITHUB_TOKEN_FILE=relative\n")):
                f = d.Fixture(parent, f"{platform}-{transport}-refuse-{name}", transport, platform, True)
                with f.envfile.open("a") as out: out.write(addition)
                before = snapshot(resources(f)); activation = state(f)
                assert run(f).returncode != 0
                assert snapshot(resources(f)) == before and state(f) == activation
                assert not recovery(f)
                assert not any(e["kind"] in ("manager", "http", "app", "dispatch", "cli-gate", "relay", "tunnel") for e in f.events())
            # An absent ancestor plus '..' must not mask an existing resource
            # until mkdir/publication. Refuse before touching either spelling.
            f = d.Fixture(parent, f"{platform}-{transport}-future-alias", transport, platform, True)
            before = snapshot(resources(f)); activation = state(f)
            f.env["HERDR_RELAY_ENV"] = str(f.root / "uncreated/../config/relay.env")
            assert run(f).returncode != 0
            assert snapshot(resources(f)) == before and state(f) == activation
            assert not (f.root / "uncreated").exists() and not recovery(f)
            assert not any(e["kind"] in ("manager", "http", "app", "dispatch", "cli-gate", "relay", "tunnel") for e in f.events())
            for raw_key in ("short", "x" * 33, "z" * 32, "é" * 16):
                f = d.Fixture(parent, f"{platform}-{transport}-key-{len(raw_key)}", transport, platform)
                d.write(f.envfile, f.envfile.read_text().replace("0123456789abcdef0123456789abcdef", raw_key))
                before = snapshot(resources(f))
                result = run(f)
                assert (result.returncode == 0) == (len(raw_key.encode()) == 32), "key is bytes, not hex"
                if result.returncode: assert snapshot(resources(f)) == before and not recovery(f)
            for spelling in ("0", "false", "False", "FALSE", "f", "F"):
                f = d.Fixture(parent, f"{platform}-{transport}-false-{spelling}", transport, platform)
                with f.envfile.open("a") as out: out.write("HERDR_RELAY_REARM_BOOTSTRAP=" + spelling + "\n")
                assert run(f).returncode == 0

            for initial in ("active", "inactive", "fresh"):
                for failure in ("", "read", "query", "snapshot", "candidate", "chmod", "token", "rename", "commit", "reload", "start", "stop",
                                "ready-stop-fail", "ready-still-active", "ready-unknown", "ready-restore", "ready-safe",
                                "public-stop-fail", "public-unknown", "public-safe", "prior-unready"):
                    if (failure == "reload" and platform == "launchd") or (failure == "stop" and initial != "active"): continue
                    if failure == "prior-unready" and initial != "active": continue
                    f = d.Fixture(parent, f"{platform}-{transport}-{initial}-{failure or 'ok'}", transport, platform, initial != "fresh")
                    if initial == "inactive":
                        d.write(f.root / "activation", json.dumps({k: dict(active=False, enabled=False) for k in ("current", "legacy")}))
                    # Identical saved raw values and supported shell-quoted
                    # fragments; ambient credentials MUST NOT be consulted.
                    with f.envfile.open("a") as out:
                        out.write("  export GH_TOKEN='fixture-native-'\"selected-secret\"\n\tGITHUB_TOKEN=" + canary + "\n")
                    if failure.startswith("public-"): f.env["FAILURE"] = "app"
                    # The stub's readiness failure answers /readyz with an error inventory for the
                    # PRIOR service at begin and, after publication, for the replacement.
                    # "prior-unready" itself matches no other fixture condition.
                    if failure == "prior-unready": f.env["FAILURE"] = "readiness"
                    f.env.update(NATIVE_FAILURE=failure, GH_TOKEN="ambient-unselected", GITHUB_TOKEN="ambient-distinct",
                                 HERDR_GITHUB_TOKEN_FILE="/ambient/not-selected")
                    sidecar = f.root / "config/github-token"
                    # Even an existing UNSELECTED migration destination is a
                    # resource: its prior bytes/mode must survive rollback.
                    if initial != "fresh":
                        d.write(sidecar, "prior-unselected-sidecar\n"); sidecar.chmod(0o640)
                    before = snapshot(resources(f)); activation = state(f)
                    protected = snapshot([f.root / "config/tunnel.yml", f.root / "config/phone-app-origin-configured", f.root / "config/devices.json",
                                          f.script.parent / "herdr-mobile-relay-service.sh"])
                    result = run(f)
                    assert snapshot(map(pathlib.Path, protected)) == protected
                    assert bystander.poll() is None, "native cleanup killed an unrelated process"
                    if not failure:
                        assert result.returncode == 0 and not f.legacy.exists() and not recovery(f)
                        assert sidecar.read_text() == canary + "\n" and stat.S_IMODE(sidecar.stat().st_mode) == 0o600
                        assert "GH_TOKEN=" not in f.envfile.read_text() and "GITHUB_TOKEN=" not in f.envfile.read_text()
                        assert state(f)["current"]["active"] and not state(f)["legacy"]["active"]
                    else:
                        assert result.returncode != 0
                        if failure in ("ready-stop-fail", "ready-still-active", "ready-unknown", "ready-restore", "public-stop-fail", "public-unknown"):
                            assert recovery(f), "unsafe/failed rollback discarded private recovery"
                            assert "rollback is incomplete" in result.stderr
                            assert f.definition.exists() and f.envfile.exists() and sidecar.exists()
                            # No prior activation follows the first failed
                            # restore; no resource mutation follows an unknown
                            # or still-active replacement observation.
                            events = f.events()
                            if failure == "ready-restore":
                                fault = next(i for i, e in enumerate(events) if e["kind"] == "fault")
                                assert not any(e["kind"] == "manager" and e["command"] in ("restart", "bootstrap") for e in events[fault + 1:])
                            else:
                                assert f.definition.read_text() != "previous definition\n"
                                assert sidecar.read_text() == canary + "\n"
                        elif failure == "stop":
                            assert snapshot(resources(f)) == before and state(f)["current"]["active"]
                            assert recovery(f) and "rollback is incomplete" in result.stderr
                        elif failure == "prior-unready":
                            # The prior service was active but its /readyz was unverifiable at begin,
                            # so no saved readiness evidence exists, and the replacement then failed
                            # readiness. Rollback restores the files but must NOT reactivate the
                            # unverified prior service: no manager enable/restart/bootstrap/kickstart
                            # after rollback starts, services stay quiesced, recovery is retained.
                            assert recovery(f) and "rollback is incomplete" in result.stderr
                            assert snapshot(resources(f)) == before
                            events = f.events()
                            # Forward activation of the replacement, then the rollback quiesce stop.
                            activated = next(i for i, e in enumerate(events)
                                             if e["kind"] == "manager" and e["command"] in ("restart", "bootstrap"))
                            rollback = next(i for i, e in enumerate(events)
                                            if i > activated and e["kind"] == "manager" and e["command"] in ("stop", "bootout"))
                            assert not any(e["kind"] == "manager" and e["command"] in ("enable", "restart", "bootstrap", "kickstart")
                                           for e in events[rollback + 1:]), "unverified prior service reactivated"
                            assert not any(s["active"] for s in state(f).values()), "unverified prior service left active"
                        else:
                            assert snapshot(resources(f)) == before and state(f) == activation
                    for r in recovery(f):
                        assert stat.S_IMODE(r.stat().st_mode) == 0o700

            # F001: the real installer has set -u and starts identity tracking
            # empty. /bin/bash is stock 3.2 on the admitted Darwin runner.
            f = d.Fixture(parent, f"{platform}-{transport}-empty-identities", transport, platform)
            result = run(f)
            assert result.returncode == 0 and "unbound variable" not in result.stderr
            assert state(f)["current"]["active"] and not f.legacy.exists() and not recovery(f)

            # F002: private metadata does not make a selected credential safe
            # to overwrite/remove as a transaction destination. With false
            # activation, launchd labels are absent even when files exist.
            for role in ("environment", "current", "legacy"):
                for prior_active in (False, True):
                    f = d.Fixture(parent, f"{platform}-{transport}-pointer-overlap-{role}-{prior_active}",
                                  transport, platform, prior_active)
                    pointer = {"environment": f.envfile, "current": f.definition, "legacy": f.legacy}[role]
                    if role != "environment": d.write(pointer, "synthetic-selected-credential\n")
                    with f.envfile.open("a") as out:
                        out.write(f"HERDR_GITHUB_TOKEN_FILE='{pointer}'\nGH_TOKEN=one\nGITHUB_TOKEN=two\n")
                    before = snapshot(resources(f)); activation = state(f)
                    result = run(f)
                    assert result.returncode != 0 and "overlaps native transaction destinations" in result.stderr
                    assert snapshot(resources(f)) == before and state(f) == activation and not recovery(f)
                    assert not any(e["kind"] in ("manager", "http", "app", "dispatch", "cli-gate", "relay", "tunnel") for e in f.events())
                    assert bystander.poll() is None

            # A valid saved private pointer wins over contradictory raw keys;
            # neither its target nor an unselected destination is rewritten.
            f = d.Fixture(parent, f"{platform}-{transport}-pointer", transport, platform)
            pointer = f.root / "private-selected"
            d.write(pointer, "selected-pointer-content\n")
            with f.envfile.open("a") as out:
                out.write(f"HERDR_GITHUB_TOKEN_FILE='{pointer}'\nGH_TOKEN=one\nGITHUB_TOKEN=two\n")
            before = snapshot([pointer, f.root / "config/github-token"])
            assert run(f).returncode == 0
            assert snapshot(map(pathlib.Path, before)) == before
            assert "GH_TOKEN=" not in f.envfile.read_text() and "GITHUB_TOKEN=" not in f.envfile.read_text()

            for missing in ("file", "key", "instance"):
                f = d.Fixture(parent, f"{platform}-{transport}-defaults-{missing}", transport, platform)
                if missing == "file":
                    f.envfile.unlink()
                    f.env.update(HERDR_RELAY_TRANSPORT=transport, HERDR_RELAY_PORT="18375")
                    if transport == "gateway": f.env["HERDR_GATEWAY_URL"] = "wss://gateway.example.test"
                else:
                    prefix = "HERDR_RELAY_TOKEN=" if missing == "key" else "HERDR_RELAY_INSTANCE_ID="
                    d.write(f.envfile, "\n".join(line for line in f.envfile.read_text().splitlines() if not line.startswith(prefix)) + "\n")
                assert run(f).returncode == 0
                lines = dict(line.split("=", 1) for line in f.envfile.read_text().splitlines() if "=" in line)
                assert len(lines["HERDR_RELAY_TOKEN"].strip("'")) == 32
                assert lines["HERDR_RELAY_INSTANCE_ID"].strip("'")

            for sig in (signal.SIGINT, signal.SIGTERM):
                f = d.Fixture(parent, f"{platform}-{transport}-cancel-{sig}", transport, platform, True)
                before = snapshot(resources(f)); activation = state(f)
                f.env["NATIVE_FAILURE"] = "cancel"
                proc = subprocess.Popen(["/bin/bash", "-x", str(f.script)], env=f.env, stdin=subprocess.DEVNULL,
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
                try:
                    deadline = time.monotonic() + 15
                    while not (f.root / "cancel-gate").exists():
                        assert proc.poll() is None and time.monotonic() < deadline, "did not reach postactivation cancellation gate"
                        time.sleep(.02)
                    proc.send_signal(sig); (f.root / "cancel-release").touch()
                    stdout, stderr = proc.communicate(timeout=20)
                    f.check_safe(stdout + stderr)
                    assert proc.returncode == 128 + sig
                    assert snapshot(resources(f)) == before and state(f) == activation
                    assert bystander.poll() is None
                finally:
                    if proc.poll() is None:
                        # Owned fallback containment, not supervisor evidence.
                        os.killpg(proc.pid, signal.SIGKILL); proc.communicate(timeout=5)
finally:
    bystander.terminate(); bystander.wait(timeout=5)
print("hosted native admission/resource/rollback fixture source exercised")
PY

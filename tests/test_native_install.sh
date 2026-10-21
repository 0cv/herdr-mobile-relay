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
# Cell selection. The default is the whole exhaustive suite. HERDR_NATIVE_INSTALL_TIER=fast|full, _SHARD=i/n and _CELL=<id>
# narrow it; an invalid value refuses here, before any build or fixture. HERDR_NATIVE_INSTALL_EVIDENCE=<new file> receives one
# line per COMPLETED cell. The plan is a pure text enumeration of this file (tests/service_qualification_matrix.py).
python3 -I -B "$REPO_DIR/tests/service_qualification_matrix.py" plan --source "$REPO_DIR/tests/test_native_install.sh" \
    --fast-cases "$REPO_DIR/tests/native_install_fast_cases.json" --output "$WORK_DIR/selection.json" || exit 2
export NATIVE_SELECTION="$WORK_DIR/selection.json"
export NATIVE_HELPER="$WORK_DIR/helper"
python3 - "$NATIVE_HELPER" "$REPO_DIR/cmd/herdr-mobile-relay" <<'PY'
import subprocess, sys
subprocess.run(["go", "build", "-o", sys.argv[1], sys.argv[2]], check=True, timeout=180)
PY
# Existing production-dispatch, diagnostic and navigation suites remain wired;
# they retain their separate acceptance boundaries and their own hosted guards.
# The service qualification harness sets HERDR_NATIVE_INSTALL_SKIP_NESTED_CHILDREN=1 because it runs all three as their
# own required suites (own exit status, timing and bound; nested, one doctor failure hid the native cells and the nested
# dispatch copy used most of this suite's wall-clock bound). Without the variable (`make shell-check`, a developer run)
# the nested execution is unchanged.
python3 - "$REPO_DIR" <<'PY'
import os, pathlib, subprocess, sys
repo = pathlib.Path(sys.argv[1])
assert os.environ.get("HERDR_NATIVE_INSTALL_SKIP_NESTED_CHILDREN") in (None, "1"), "SKIP_NESTED_CHILDREN must be unset or 1"
for name, timeout in (("test_service_transport_dispatch.py", 900),
                      ("test_plugin_doctor.py", 120), ("test_setup_menu.py", 180)):
    if os.environ.get("HERDR_NATIVE_INSTALL_SKIP_NESTED_CHILDREN") == "1":
        print(name + " runs as its own qualification suite; not nested here", flush=True)
        continue
    subprocess.run([sys.executable, str(repo / "tests" / name)], check=True, timeout=timeout)
PY

# Reuse the actual-copy fixture, strict compiled helper and dynamic, inert
# label-specific controllers. No duplicated/hand-copied production policy.
python3 - "$REPO_DIR" "$WORK_DIR" <<'PY'
import collections, hashlib, importlib.util, json, os, pathlib, re, signal, stat, subprocess, sys, time
from contextlib import contextmanager
repo, parent = map(pathlib.Path, sys.argv[1:])
spec = importlib.util.spec_from_file_location("dispatch_fixture", repo / "tests/test_service_transport_dispatch.py")
d = importlib.util.module_from_spec(spec)
spec.loader.exec_module(d)
# These finite adapters use only the standard library. Avoid site initialization
# on EVERY tool invocation, and import signal only for the long-lived worker.
# subprocess is unused in FAKE. All controller/filesystem/fault logic stays exact;
# standalone dispatch fixtures keep their original adapter and interpreter.
imports = "import json, os, pathlib, signal, subprocess, sys, time"
assert d.FAKE.count(imports) == 1 and d.FAKE.count("def worker(kind):") == 1
assert "subprocess." not in d.FAKE
d.FAKE = d.FAKE.replace(imports, "import json, os, pathlib, sys, time", 1).replace(
    "def worker(kind):", "def worker(kind):\n    import signal", 1)
dispatch_write = d.write

# Process cost. A cell starts the Python adapter ~140 times (about 25 ms each, most of it the interpreter), and
# three of its branches only append ONE event and exec the real tool (or stand in for sleep). A bash prelude answers
# exactly those calls; every other call (each fault name, any live-resource destination, any other argv) runs the
# adapter itself, unchanged. The prelude mirrors the adapter text pinned below, so editing those branches fails here
# until the prelude is re-derived, and native_shim_parity replays representative calls through both.
ADAPTERS = parent / "native-shims"
FAST_TOOLS = ("sleep", "cp", "mv", "rm", "chmod", "mktemp", "cat", "head", "herdr-mobile-relay")
FAST_SHIM = r'''#!/bin/bash
name="${0##*/}"
adapter="@ADAPTERS@/$name"
[ -n "${FIXTURE_ROOT-}" ] || exec "$adapter" "$@"
is_live() {
    case "$1" in
        "$FIXTURE_ROOT/config/relay.env"|"$FIXTURE_ROOT/config/github-token"|\
        "$FIXTURE_ROOT/.config/systemd/user/herdr-mobile-relay.service"|"$FIXTURE_ROOT/.config/systemd/user/herdr-remote.service"|\
        "$FIXTURE_ROOT/Library/LaunchAgents/com.herdr-mobile-relay.service.plist"|\
        "$FIXTURE_ROOT/Library/LaunchAgents/com.herdr-remote.service.plist") return 0 ;;
    esac
    return 1
}
case "$name" in
    sleep)
        case "$#:${1-}" in
            1:1|1:0.1) exec /bin/sleep 0.05 ;;
            1:0) exit 0 ;;
        esac ;;
    herdr-mobile-relay)
        case "${1-}" in
            json-field|normalize-origin|verify-readiness|verify-gateway-health)
                [ -n "${NATIVE_HELPER-}" ] && printf '{"kind": "query", "command": "%s"}\n' "$1" >> "$FIXTURE_ROOT/events" &&
                    exec "$NATIVE_HELPER" "$@" ;;
            version)
                if [ "$#:${2-}" = 2:--json ]; then printf '%s\n' '{"version":"version","revision":"revision"}'; exit 0; fi ;;
        esac ;;
    *)
        case "${NATIVE_FAILURE-}" in candidate|snapshot|chmod|token|rename|commit|ready-restore|read) exec "$adapter" "$@" ;; esac
        live=false
        last=
        for last in "$@"; do :; done
        case "$name" in
            rm|chmod) for argument in "$@"; do if is_live "$argument"; then live=true; fi; done ;;
            *) if [ "$#" -gt 0 ] && is_live "$last"; then live=true; fi ;;
        esac
        case "$name" in cp|mv|rm|chmod) [ "$live" = false ] || exec "$adapter" "$@" ;; esac
        case "$name" in mktemp|head) tool=/usr/bin/$name ;; *) tool=/bin/$name ;; esac
        printf '{"kind": "filesystem", "tool": "%s", "live": %s}\n' "$name" "$live" >> "$FIXTURE_ROOT/events" &&
            exec -a "$name" "$tool" "$@" ;;
esac
exec "$adapter" "$@"
'''


def adapter_branch(start, end):
    first = d.FAKE.index(start)
    return d.FAKE[first:d.FAKE.index(end, first)]


assert hashlib.sha256("".join((
    adapter_branch('if name == "herdr-mobile-relay":', 'if name == "cloudflared":'),
    adapter_branch('if name == "sleep":', 'if name == "id":'),
    adapter_branch('if name in ("cp", "mv", "rm", "chmod", "mktemp", "cat", "head"):', 'if name == "plutil":'))).encode()
).hexdigest() == "69d5841de2e35feb31937409d63adb0b815ec6a274f0122f9c5c6f253e918909", (
    "the shared adapter changed: re-derive FAST_SHIM in tests/test_native_install.sh")


def native_fixture_write(path, text, executable=False):
    if executable and text == "#!" + sys.executable + "\n" + d.FAKE:
        text = text.replace("\n", " -S\n", 1)
        if path.name in FAST_TOOLS:
            # The adapter itself (the very text written above) is kept once for the whole suite, where the prelude
            # hands back every call it does not answer; the fixture's own copy of the tool is the prelude.
            if not (ADAPTERS / path.name).exists():
                dispatch_write(ADAPTERS / path.name, text, True)
            text = FAST_SHIM.replace("@ADAPTERS@", str(ADAPTERS))
    dispatch_write(path, text, executable)


d.write = native_fixture_write
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
interrupted = [0]


def record_interrupt(signum, _frame):
    if not interrupted[0]: interrupted[0] = signum


for sig in (signal.SIGINT, signal.SIGTERM):
    signal.signal(sig, record_interrupt)

# Cell selection (see `plan` above). Every cell starts with `cell(id)`. Reaching the next gate proves every assertion of
# the open cell passed, so only then is it recorded as completed; a failing cell raises and is never recorded. The ids
# reached, selected or not, must equal the statically enumerated inventory: the loops cannot drift from it unnoticed.
selection = json.loads(pathlib.Path(os.environ["NATIVE_SELECTION"]).read_text())
selected, reached, completed, open_cell = set(selection["selected"]), [], [], [None]
evidence = open(os.environ["HERDR_NATIVE_INSTALL_EVIDENCE"], "x", buffering=1) if os.environ.get("HERDR_NATIVE_INSTALL_EVIDENCE") else None


def close_cell():
    if open_cell[0] is not None:
        completed.append(open_cell[0])
        if evidence: print(open_cell[0], file=evidence, flush=True)
        open_cell[0] = None


def cell(cell_id):
    close_cell()
    reached.append(cell_id)
    if cell_id not in selected: return False
    open_cell[0] = cell_id
    return True


@contextmanager
def owned_process(command, limit, **options):
    # The anchor has no child pipes. Keep it unreaped until its group is
    # signalled, even if Popen fails or the command leader exits first.
    anchor = proc = None
    try:
        if interrupted[0]: raise SystemExit(128 + interrupted[0])
        anchor = subprocess.Popen([sys.executable, "-I", "-B", "-c",
                                   f"import time; time.sleep({limit + 60})"],
                                  stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                  stderr=subprocess.DEVNULL, close_fds=True, process_group=0)
        if interrupted[0]: raise SystemExit(128 + interrupted[0])
        proc = subprocess.Popen(command, process_group=anchor.pid, **options)
        yield proc
    finally:
        if anchor is not None:
            try:
                os.killpg(anchor.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            finally:
                try:
                    if proc is not None:
                        try:
                            # collect() already bounded the required output. On
                            # failure discard any unfinished pipes after killing
                            # the group rather than wait on inherited writers.
                            for stream in (proc.stdin, proc.stdout, proc.stderr):
                                if stream is not None and not stream.closed: stream.close()
                        finally:
                            proc.wait(timeout=5)
                finally:
                    anchor.wait(timeout=5)
    if interrupted[0]: raise SystemExit(128 + interrupted[0])


def collect(proc, limit, data=None):
    deadline = time.monotonic() + limit
    while True:
        if interrupted[0]: raise SystemExit(128 + interrupted[0])
        remaining = deadline - time.monotonic()
        if remaining <= 0: raise subprocess.TimeoutExpired(proc.args, limit)
        try:
            # Short waits allow recorded cancellation to reach owned cleanup;
            # Popen retains partial output/input between communicate retries.
            return proc.communicate(input=data, timeout=min(.25, remaining))
        except subprocess.TimeoutExpired:
            if time.monotonic() >= deadline: raise subprocess.TimeoutExpired(proc.args, limit)
            data = None


def run(f, conditional=""):
    # Ordinary cells use the REAL installer. Conditional cells call the REAL
    # transaction API with a conditional EXIT trap, not a conditional source
    # of the whole installer (which would mask unrelated public-gate errexit).
    started = time.monotonic()
    command = ["/bin/bash", "-x", str(f.script)]
    if conditional:
        command = ["/bin/bash", "-x", "-c", NATIVE_EXIT_PROBE, str(f.script), str(f.script.parent), f.platform,
                   str(f.definition), str(f.legacy), conditional]
    try:
        with owned_process(command, 40, env=f.env, stdin=subprocess.DEVNULL,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) as proc:
            stdout, stderr = collect(proc, 40)
            result = subprocess.CompletedProcess(command, proc.returncode, stdout, stderr)
            f.check_safe(stdout + stderr)
            assert canary not in stdout + stderr, "native credential exposed"
            return result
    finally:
        # Progress evidence only: a suite that reaches its wall-clock bound otherwise
        # leaves no trace of how many cells ran or how long each took. The tally counts the
        # adapter calls the fixture logged (one event kind per call; sleep is not logged), by tool.
        NATIVE_RUNS[0] += 1
        try:
            logged = f.events()
        except ValueError:
            logged = []
        named = {"http": "curl", "query": "relay", "app": "relay", "cli-gate": "relay"}
        spawns = collections.Counter(named.get(e["kind"]) or e["tool"] for e in logged
                                     if e["kind"] in ("manager", "filesystem", *named))
        print("native installation fixture invocation %d (%s) took %.1fs; stub spawns %d %s" % (
            NATIVE_RUNS[0], f.platform, time.monotonic() - started, sum(spawns.values()), dict(sorted(spawns.items()))),
            file=sys.stderr, flush=True)


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
    with owned_process(command, 20, env=f.env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                       stderr=subprocess.PIPE) as proc:
        stdout, stderr = collect(proc, 20, data)
        f.check_safe((stdout + stderr).decode())
        return proc.returncode, stdout


def capture_control(f, seam, role, payload, status=0, when="initial", at=1):
    d.write(f.root / "native-capture.json", json.dumps(dict(seam=seam, label=role, hex=payload.hex(), status=status, when=when, at=at)))


def native_modern_not_found_control():
    # Task Z. Not a cell (the frozen inventory of tests/service_qualification_matrix.py does not grow): the REAL native_install_observe, sourced from the repository, reads modern launchctl's two-line
    # answer for a label that is not loaded. A nonzero label query is absence only when its merged `2>&1` text, after ONE terminating LF, is exactly the diagnostic (macOS 12 and earlier),
    # `Bad request.` + LF + the diagnostic, or the diagnostic + LF + `Bad request.` (a stdout line buffered under `2>&1` can follow an unbuffered stderr one); every other text, and any status of 0, is unknown.
    label, uid = "com.herdr-mobile-relay.service", str(os.getuid())
    diag = lambda name, user=uid: 'Could not find service "' + name + '" in domain for user gui: ' + user
    ours, bad, framed = diag(label), "Bad request.", "gui/" + uid + "/" + label + " = {\n}\n"
    script = r'''set +e
native_manager=launchd
. "$REPO/relay/common.sh"
. "$REPO/relay/native-install-transaction.sh"
launchctl() { case "$1" in print) case "$2" in gui/*/*) printf '%s' "$OUT"; printf '%s' "$ERR" >&2; return "$CODE" ;; *) printf '%s = {}' "$2" ;; esac ;; print-disabled) printf 'disabled services = {}\n' ;; esac; }
if native_install_observe "$LABEL"; then echo "$NATIVE_STATE:$NATIVE_ENABLED"; else echo unknown:unknown; fi
'''
    # (stdout, stderr, status): the product merges the two streams, stdout first
    accepted = [("", ours + "\n", 113), (ours + "\n", "", 113), ("", bad + "\n" + ours + "\n", 113), (bad + "\n" + ours + "\n", "", 113), ("", ours + "\n" + bad + "\n", 113),
                ("", bad + "\n" + ours, 1), (bad + "\n", ours + "\n", 113), (ours + "\n", bad + "\n", 255)]
    refused = [("", bad + "\n\n" + ours + "\n", 113), (bad + "\n\n", ours + "\n", 113),  # a blank line between the two
               ("", bad + "\n" + diag("com.example.other") + "\n", 113), ("", bad + "\n" + diag(label, str(os.getuid() + 1)) + "\n", 113),  # another label, another uid
               ("", bad + "\n" + ours + "\n", 0), (bad + "\n" + ours + "\n", "", 0), ("", ours + "\n", 0),  # a status of 0 is no not-found diagnostic
               ("", bad + "\n" + bad + "\n" + ours + "\n", 113), ("", bad + "\r\n" + ours + "\n", 113), ("", bad + " " + ours + "\n", 113), ("", "bad request.\n" + ours + "\n", 113),
               ("", bad + "\n" + ours + "\nextra\n", 113), ("", bad + "\n" + ours + "\n\n", 113), ("", bad + "\n", 113), ("", "", 113),
               (framed, "", 113), (bad + "\n" + framed, "", 113)]  # a loaded answer is never absence, whatever the status
    # The grammar variants, byte for byte those of tests/test_native_manager_lifecycle_matrix.py (NOT_FOUND_ACCEPTED and NOT_FOUND_REFUSED; a matrix test compares them, so the three never drift): {b} is `Bad request.`,
    # {d} the exact diagnostic of this label and uid, {dl} the diagnostic of another label, {du} of another uid, {dc} a cut diagnostic and {dlow} the lower-cased one.
    NOT_FOUND_ACCEPTED = ("{d}", "{d}\n", "{b}\n{d}", "{b}\n{d}\n", "{d}\n{b}", "{d}\n{b}\n")
    NOT_FOUND_REFUSED = (
        "{b}", "{b}\n", "", "\n", "\n\n",  # the other line alone, nothing at all
        "{b}\n\n{d}", "{b}\n\n{d}\n", "{d}\n\n{b}", "{d}\n\n{b}\n",  # a blank line between the two
        "{b}\n{b}\n{d}", "{b}\n{d}\n{d}", "{b}\n{d}\n{b}", "{d}\n{b}\n{d}", "{d}\n{d}", "{d}\n{d}\n{b}", "{b}\n{b}",  # a repeated line
        "{b}\r\n{d}", "{b}\n{d}\r", "{b}\r\n{d}\r\n", "{d}\r\n{b}", "{d}\n{b}\r", "{b}\r{d}",  # a CR
        "{b} {d}", "{b}{d}", "{d} {b}", "{d}{b}", "{b}\n{d} {b}",  # the two on one line
        "bad request.\n{d}", "BAD REQUEST.\n{d}", "Bad Request.\n{d}", "{b}\n{dlow}", "{dlow}", "Bad request\n{d}", "Bad request..\n{d}", "{b}.\n{d}",  # case and spelling
        "{b}\n{dl}", "{dl}\n{b}", "{b}\n{du}", "{du}\n{b}", "{b}\n{dc}", "{dc}",  # another label, another uid, a cut diagnostic
        "{b}\n{d}\nextra", "extra\n{b}\n{d}", "{b}\nextra\n{d}", "{b}\n{d} ", " {b}\n{d}", "{b} \n{d}", "{b}\n {d}", "\n{b}\n{d}", "{d}\n{b}\nextra",  # extra text or blanks
        "{b}\n{d}\n\n", "{d}\n{b}\n\n", "{b}\n{d}\n\n\n", "{d}\n\n")  # a second trailing LF
    fill = lambda template: template.format(b=bad, d=ours, dl=diag("com.example.other"), du=diag(label, str(os.getuid() + 1)), dc=ours[:-1], dlow=ours.lower())
    # The whole text arrives merged (here on stderr; the product reads `2>&1`): every accepted form at status 113 (and at 1 and 255 for three), none at status 0; every refused variant at 113, and five of them at 0, 2 and 255.
    accepted += [("", fill(x), code) for x in NOT_FOUND_ACCEPTED for code in ((113, 1, 255) if x in ("{d}", "{b}\n{d}\n", "{d}\n{b}\n") else (113,))]
    refused += [("", fill(x), 113) for x in NOT_FOUND_REFUSED] + [("", fill(x), 0) for x in NOT_FOUND_ACCEPTED]
    refused += [("", fill(x), code) for x in ("{b}\n\n{d}\n", "{b}\n{dl}", "{b}\n{du}", "{d}\n{b}\r", "{b}\n{d}\n\n") for code in (0, 2, 255)]
    # The same texts split across the two streams (stdout first, as the merge orders them): the accepted forms stay absence at the cut, the refused ones stay refused.
    for x, first in (("{b}\n{d}\n", "{b}\n"), ("{d}\n{b}\n", "{d}\n"), ("{b}\n{d}", "{b}\n"), ("{d}\n{b}", "{d}\n"), ("{d}\n", "{d}")):
        accepted.append((fill(x)[:len(fill(first))], fill(x)[len(fill(first)):], 113))
    for x in ("{b}\n\n{d}\n", "{b}\n{dl}\n", "{b}\n{du}\n", "{b}\n{d}\n\n"):
        refused.append((fill(x)[:fill(x).index("\n") + 1], fill(x)[fill(x).index("\n") + 1:], 113))
    for expected, cases in (("absent:true", accepted), ("unknown:unknown", refused)):
        for out, err, code in cases:
            done = subprocess.run(["/bin/bash", "--noprofile", "--norc", "-c", script], capture_output=True, cwd=repo, timeout=30,
                                  env={"PATH": "/usr/bin:/bin", "HOME": str(parent), "REPO": str(repo), "LABEL": label, "OUT": out, "ERR": err, "CODE": str(code)})  # the observer writes nothing
            assert done.stdout.decode().strip() == expected, ("modern not-found answer", out, err, code, done.stdout, done.stderr[:200])


OVER_BOUND_LISTING = b"gui/fixture = {\n" + b"\t\t00001\t-\tcom.example.agent\n" * 2340 + b"}"  # 65,537 bytes


def capture_frames(platform, role):
    label = ("herdr-mobile-relay.service" if role == "current" else "herdr-remote.service") if platform == "systemd" else (
        "com.herdr-mobile-relay.service" if role == "current" else "com.herdr-remote.service")
    if platform == "systemd":
        return label, [("systemd", b"LoadState=loaded\nActiveState=inactive\nUnitFileState=disabled\n", 0, b"inactive", 4096)]
    negative = ('Could not find service "' + label + '" in domain for user gui: ' + str(os.getuid())).encode()
    # The current label answers as macOS 13+ does (`Bad request.`, then the diagnostic) and the legacy label as macOS 12 and earlier (the diagnostic alone): both are exact absence, as in the fixture's own adapter.
    if role == "current": negative = b"Bad request.\n" + negative
    # The current label's table has the macOS 13+ layout (indented, keyword states, a trailing login-item section); the legacy label's keeps the macOS 12 one (single blank, true|false).
    disabled = ('\tdisabled services = {\n\t\t"' + label + '" => disabled\n\t}\n\n\tlogin item associations = {\n\t}' if role == "current" else
                'disabled services = {\n "' + label + '" => true\n}').encode()
    return label, [("domain", b"gui/fixture = {}", 0, b"fixture", 65536),
                   ("label", negative, 113, b"service", 65536),
                   ("disabled", disabled, 0, b"disabled" if role == "current" else b"true", 65536)]


def capture_faults(seam, payload, status, part, limit):
    faults = [("middle-nul", payload.replace(part, part[:2] + b"\x00" + part[2:], 1), status),
              ("prefix-nul", b"\x00" + payload, status), ("suffix-nul", payload + b"\x00", status),
              ("oversized", b"x" * (limit + 1), status),
              # An exact nonzero launchd absence is SUPPORTED, not a failed
              # capture. Failed label probes must have a nonmatching diagnostic.
              ("failed", b"controller unavailable" if seam == "label" else payload, 2),
              ("partial", payload[:len(payload) // 2], 2), ("empty", b"", 2)]
    # The domain listing is read by STATUS alone (its text is discarded, so a NUL or an over-limit listing is no fault): every fault of that seam is a failing query.
    return [(name, data, 2) for name, data, code in faults] if seam == "domain" else faults


def native_capture_cases(parent, platform, transport):
    for role in ("current", "legacy"):
        label, frames = capture_frames(platform, role)
        for seam, payload, status, part, limit in frames:
            faults = capture_faults(seam, payload, status, part, limit)
            for name, data, code in faults:
                for invocation in ("observe-if", "observe-or", "quiet-if", "quiet-or"):
                    cell_id = f"capture-probe-{platform}-{transport}-{role}-{seam}-{name}-{invocation}"
                    if not cell(cell_id): continue
                    f = d.Fixture(parent, cell_id, transport, platform)
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
                cells += [("fresh", number, "middle-nul", faults[0][1], faults[0][2], "") for number in (2, 3, 4)]
                cells += [(phase, 1, name, data, code, conditional)
                          for phase in ("ready", "public")
                          for name, data, code in faults[:6]
                          for conditional in ("", "if", "or")]
                for phase, at, name, data, code, conditional in cells:
                    cell_id = f"capture-exit-{platform}-{transport}-{role}-{seam}-{initial}-{phase}-{at}-{name}-{conditional}"
                    if not cell(cell_id): continue
                    f = d.Fixture(parent, cell_id, transport, platform, initial)
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
        cell_id = f"capture-valid-{platform}-{transport}-{role}"
        if not cell(cell_id): continue
        f = d.Fixture(parent, cell_id, transport, platform)
        result, output = probe(f, label, "quiet-if", "supported")
        assert result == 0 and output in (b"absent:false\n", b"inactive:false\n")
        for seam, payload, code, part, limit in frames:
            # The domain listing is read by status alone: one over the old 65,536-byte bound (65,537 bytes, the most a control carries; macOS 15 prints 89,044) is as good as a short one.
            capture_control(f, seam, role, OVER_BOUND_LISTING if seam == "domain" else payload.rstrip(b"\n"), code)
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
            cell_id = f"origin-{platform}-{role}-{kind}"
            if not cell(cell_id): continue
            f = d.Fixture(parent, cell_id, "gateway", platform, kind != "foreign-bare")
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
    if cell(f"origin-{platform}-match"):
        f = d.Fixture(parent, f"origin-{platform}-match", "gateway", platform, True)
        result = run(f)
        assert result.returncode == 0 and "Native installation refused" not in result.stderr
        assert set(origin_queries(f)) == {label_of("current"), label_of("legacy")}
    if cell(f"origin-{platform}-fresh"):
        f = d.Fixture(parent, f"origin-{platform}-fresh", "gateway", platform)
        result = run(f)
        assert result.returncode == 0 and "Native installation refused" not in result.stderr
        if platform == "systemd": assert not origin_queries(f)
    if cell(f"origin-input-{platform}"): native_origin_input_cases(parent, platform)


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


def native_shim_parity():
    # The bash prelude must be indistinguishable from the adapter it shortcuts. Replay representative calls, including
    # each condition it must hand back (fault names, live destinations, other argv), through the prelude in one fixture
    # and through the shared adapter itself in an identical one: status, output, new events and the file tree (path, mode, size and content digest) must match.
    fast, slow = (d.Fixture(parent, "shim-parity-" + variant) for variant in ("fast", "adapter"))
    assert all((f.bin / "cp").read_bytes().startswith(b"#!/bin/bash") for f in (fast, slow))
    live, plain, relay = ".config/systemd/user/herdr-mobile-relay.service", "{r}/config/copy", "{r}/config/relay.env"
    resources = (live, ".config/systemd/user/herdr-remote.service", "Library/LaunchAgents/com.herdr-mobile-relay.service.plist",
                 "Library/LaunchAgents/com.herdr-remote.service.plist", "config/relay.env", "config/github-token")
    calls = [("sleep", [a], {}, False) for a in ("1", "0.1", "0", "0.01")] + [
        ("sleep", ["0.3", "x"], {}, False), ("sleep", ["x"], {}, False), ("sleep", [], {}, False),
        ("herdr-mobile-relay", ["version", "--json"], {}, False), ("herdr-mobile-relay", ["version"], {}, False),
        ("herdr-mobile-relay", [], {}, False), ("herdr-mobile-relay", ["unknown"], {}, False),
        ("herdr-mobile-relay", ["json-field", "string", "key"], {}, False),
        ("cat", ["{r}/config/devices.json"], {}, False), ("cat", [relay], {}, False), ("cat", ["{r}/absent"], {}, False),
        ("head", ["-c", "4", relay], {}, False), ("head", ["-c", "262145", relay], {"NATIVE_FAILURE": "read"}, False),
        ("mktemp", ["{r}/config/t.XXXXXX"], {}, False), ("mktemp", ["-d", "{r}/config/t.XXXXXX"], {}, False),
        ("cp", ["-p", "{r}/config/devices.json", plain], {}, False),
        ("cp", ["-p", "{r}/config/devices.json", "{r}/config/candidate.env"], {"NATIVE_FAILURE": "candidate"}, False),
        ("cp", ["-p", "{r}/config/devices.json", "{r}/" + live], {}, False),
        ("cp", ["-p", "{r}/config/devices.json", "{r}/" + live], {}, True),
        ("chmod", ["600", plain], {}, False), ("chmod", ["600", relay], {}, False),
        ("chmod", ["600", "{r}/config/.relay-env.x"], {"NATIVE_FAILURE": "chmod"}, False),
        ("mv", ["-f", plain, "{r}/config/moved"], {}, False), ("mv", ["-f", "{r}/config/moved", relay], {"NATIVE_FAILURE": "rename"}, False),
        ("rm", ["-f", "{r}/config/moved"], {}, False), ("rm", ["-f", "{r}/" + live], {}, False),
        ("rm", ["-f", "{r}/config/herdr-remote.x"], {"NATIVE_FAILURE": "commit"}, False),
        ("cp", ["-p", "{r}/config/devices.json", "{r}/config/0"], {"NATIVE_FAILURE": "snapshot"}, False),
        ("chmod", ["600", "{r}/config/candidate.token"], {"NATIVE_FAILURE": "token"}, False),
        ("cp", ["-p", "{r}/config/0", "{r}/config/restore"], {"NATIVE_FAILURE": "ready-restore"}, False),
        ("rm", ["-f", "{r}/" + live], {"NATIVE_FAILURE": "ready-restore"}, False)]
    # With the manager active the adapter's quiescence assertion must still run: for each of the six live resources, for every tool
    # that can write one (live not last for rm and for chmod); and relay calls that only resemble an answered one must still reach the adapter.
    calls += [("chmod", ["600", "{r}/" + path], {}, True) for path in resources] + [
        ("mv", ["-f", plain, relay], {}, True), ("rm", ["-f", "{r}/" + live, "{r}/config/moved"], {}, True),
        ("chmod", ["600", "{r}/" + live, plain], {}, True),
        ("herdr-mobile-relay", ["version", "--json", "x"], {}, False), ("herdr-mobile-relay", ["version", "x"], {}, False),
        ("herdr-mobile-relay", ["verify-public", "x"], {}, False), ("herdr-mobile-relay", ["tailscale-cli", "activation-check"], {"ACTIVATION_RESULT": "3"}, False)]
    # Every fault name the prelude hands back must be replayed in a call on which the adapter really injects it.
    handed_back = set(re.search(r'case "\$\{NATIVE_FAILURE-\}" in ([a-z|-]+)\)', FAST_SHIM).group(1).split("|"))
    injected, offsets = set(), {}
    for tool, argv, extra, active in calls:
        observed = []
        for f, via_prelude in ((fast, True), (slow, False)):
            program = ADAPTERS / tool
            if via_prelude:
                program = f.root / "releases/current" / tool if tool == "herdr-mobile-relay" else f.bin / tool
            d.write(f.root / "activation", json.dumps({k: dict(active=active, enabled=active) for k in ("current", "legacy")}))
            (f.root / "injected").unlink(missing_ok=True)
            done = subprocess.run([str(program), *[a.replace("{r}", str(f.root)) for a in argv]], input=b'{"key":"value"}',
                                  env=dict(f.env, **extra), capture_output=True, timeout=20)
            events = (f.root / "events").read_text() if (f.root / "events").exists() else ""
            tree = sorted((str(p.relative_to(f.root)), stat.S_IMODE(p.stat().st_mode), p.stat().st_size, hashlib.sha256(p.read_bytes()).hexdigest())
                          for p in f.root.rglob("*") if p.is_file() and p.relative_to(f.root).parts[0] not in (".local", "releases", "events"))
            normal = lambda text: re.sub(r"t\.\w{6}", "t.XXXXXX", str(text).replace(str(f.root), "<root>"))
            observed.append((done.returncode, normal(done.stdout), normal(done.stderr), normal(events[offsets.get(f.root, 0):]), normal(tree)))
            offsets[f.root] = len(events)
        assert observed[0] == observed[1], (tool, argv, extra, active, observed)
        if '"kind": "fault"' in observed[1][3] or "partial-private" in observed[1][1]:
            injected.add(extra["NATIVE_FAILURE"])
    assert offsets[fast.root] and offsets[fast.root] == offsets[slow.root], "parity replay recorded no events"
    assert injected == handed_back, ("fault names without a replayed injection", sorted(handed_back ^ injected))


native_modern_not_found_control()
bystander = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(3600)"], start_new_session=True)
try:
    if cell("shim-parity"): native_shim_parity()
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
                cell_id = f"{platform}-{transport}-refuse-{name}"
                if not cell(cell_id): continue
                f = d.Fixture(parent, cell_id, transport, platform, True)
                with f.envfile.open("a") as out: out.write(addition)
                before = snapshot(resources(f)); activation = state(f)
                assert run(f).returncode != 0
                assert snapshot(resources(f)) == before and state(f) == activation
                assert not recovery(f)
                assert not any(e["kind"] in ("manager", "http", "app", "dispatch", "cli-gate", "relay", "tunnel") for e in f.events())
            # An absent ancestor plus '..' must not mask an existing resource
            # until mkdir/publication. Refuse before touching either spelling.
            cell_id = f"{platform}-{transport}-future-alias"
            if cell(cell_id):
                f = d.Fixture(parent, cell_id, transport, platform, True)
                before = snapshot(resources(f)); activation = state(f)
                f.env["HERDR_RELAY_ENV"] = str(f.root / "uncreated/../config/relay.env")
                assert run(f).returncode != 0
                assert snapshot(resources(f)) == before and state(f) == activation
                assert not (f.root / "uncreated").exists() and not recovery(f)
                assert not any(e["kind"] in ("manager", "http", "app", "dispatch", "cli-gate", "relay", "tunnel") for e in f.events())
            for raw_key in ("short", "x" * 33, "z" * 32, "é" * 16):
                cell_id = f"{platform}-{transport}-key-{len(raw_key)}"
                if not cell(cell_id): continue
                f = d.Fixture(parent, cell_id, transport, platform)
                d.write(f.envfile, f.envfile.read_text().replace("0123456789abcdef0123456789abcdef", raw_key))
                before = snapshot(resources(f))
                result = run(f)
                assert (result.returncode == 0) == (len(raw_key.encode()) == 32), "key is bytes, not hex"
                if result.returncode: assert snapshot(resources(f)) == before and not recovery(f)
            for number, spelling in enumerate(("0", "false", "False", "FALSE", "f", "F")):
                # The number keeps the directories distinct on a case-insensitive volume (the Darwin runner's default),
                # where false-False and false-FALSE would otherwise be one directory and the second mkdir an error.
                cell_id = f"{platform}-{transport}-false-{number}-{spelling}"
                if not cell(cell_id): continue
                f = d.Fixture(parent, cell_id, transport, platform)
                with f.envfile.open("a") as out: out.write("HERDR_RELAY_REARM_BOOTSTRAP=" + spelling + "\n")
                assert run(f).returncode == 0

            for initial in ("active", "inactive", "fresh"):
                for failure in ("", "read", "query", "snapshot", "candidate", "chmod", "token", "rename", "commit", "reload", "start", "stop",
                                "ready-stop-fail", "ready-still-active", "ready-unknown", "ready-restore", "ready-safe",
                                "public-stop-fail", "public-unknown", "public-safe", "prior-unready"):
                    if (failure == "reload" and platform == "launchd") or (failure == "stop" and initial != "active"): continue
                    if failure == "prior-unready" and initial != "active": continue
                    cell_id = f"{platform}-{transport}-{initial}-{failure or 'ok'}"
                    if not cell(cell_id): continue
                    f = d.Fixture(parent, cell_id, transport, platform, initial != "fresh")
                    if initial == "fresh" and failure == "snapshot":
                        # The snapshot fault seam fails cp of resource 0 (the current
                        # definition). A wholly absent fresh definition has no cp /0;
                        # keep both labels inactive but supply a dormant owned file
                        # so this cell actually reaches the snapshot failure.
                        d.write(f.definition, "previous definition\n")
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
                        assert result.returncode != 0, (platform, transport, initial, failure, result.returncode)
                        if failure == "snapshot":
                            assert any(e["kind"] == "fault" and e["point"] == "snapshot" for e in f.events()), (
                                "snapshot fault injection was not exercised", platform, transport, initial)
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
            cell_id = f"{platform}-{transport}-empty-identities"
            if cell(cell_id):
                f = d.Fixture(parent, cell_id, transport, platform)
                result = run(f)
                assert result.returncode == 0 and "unbound variable" not in result.stderr
                assert state(f)["current"]["active"] and not f.legacy.exists() and not recovery(f)

            # F002: private metadata does not make a selected credential safe
            # to overwrite/remove as a transaction destination. With false
            # activation, launchd labels are absent even when files exist.
            for role in ("environment", "current", "legacy"):
                for prior_active in (False, True):
                    cell_id = f"{platform}-{transport}-pointer-overlap-{role}-{prior_active}"
                    if not cell(cell_id): continue
                    f = d.Fixture(parent, cell_id, transport, platform, prior_active)
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
            cell_id = f"{platform}-{transport}-pointer"
            if cell(cell_id):
                f = d.Fixture(parent, cell_id, transport, platform)
                pointer = f.root / "private-selected"
                d.write(pointer, "selected-pointer-content\n")
                with f.envfile.open("a") as out:
                    out.write(f"HERDR_GITHUB_TOKEN_FILE='{pointer}'\nGH_TOKEN=one\nGITHUB_TOKEN=two\n")
                before = snapshot([pointer, f.root / "config/github-token"])
                assert run(f).returncode == 0
                assert snapshot(map(pathlib.Path, before)) == before
                assert "GH_TOKEN=" not in f.envfile.read_text() and "GITHUB_TOKEN=" not in f.envfile.read_text()

            for missing in ("file", "key", "instance"):
                cell_id = f"{platform}-{transport}-defaults-{missing}"
                if not cell(cell_id): continue
                f = d.Fixture(parent, cell_id, transport, platform)
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
                cell_id = f"{platform}-{transport}-cancel-{sig}"
                if not cell(cell_id): continue
                f = d.Fixture(parent, cell_id, transport, platform, True)
                before = snapshot(resources(f)); activation = state(f)
                f.env["NATIVE_FAILURE"] = "cancel"
                command = ["/bin/bash", "-x", str(f.script)]
                with owned_process(command, 20, env=f.env, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) as proc:
                    deadline = time.monotonic() + 15
                    while not (f.root / "cancel-gate").exists():
                        if interrupted[0]: raise SystemExit(128 + interrupted[0])
                        assert proc.poll() is None and time.monotonic() < deadline, "did not reach postactivation cancellation gate"
                        time.sleep(.02)
                    proc.send_signal(sig); (f.root / "cancel-release").touch()
                    stdout, stderr = collect(proc, 20)
                    f.check_safe(stdout + stderr)
                    assert proc.returncode == 128 + sig
                    assert snapshot(resources(f)) == before and state(f) == activation
                    assert bystander.poll() is None
    close_cell()
    assert reached == selection["inventory"], "the cells reached differ from the enumerated inventory"
    assert sorted(completed) == sorted(selected), "the completed cells differ from the selected cells"
    print("native cells completed: %d of %d (tier %s, shard %s/%s)" % (len(completed), len(reached), selection["tier"],
          selection["shard_index"], selection["shard_count"]), flush=True)
finally:
    bystander.terminate(); bystander.wait(timeout=5)
if interrupted[0]: raise SystemExit(128 + interrupted[0])
print("hosted native admission/resource/rollback fixture source exercised")
PY

"""S10B release-identity driver: setup-link arms only a coherent release.

Run only through S10B/run-checks.sh. The HERDR_S10B_SANDBOX guard prevents
accidental host execution; bwrap, not that guard, supplies filesystem, PID and
network isolation. The real product script (`relay/setup-link.sh`) is an exact
copy of the candidate (replaced together with `common.sh` and `tailscale.sh` by
the pre-fix overlay in regression mode) and it drives the frozen post-S7B1
holder/reprint binary mounted read-only at /hold-bin. The arm acknowledgement
is answered by a real Unix-socket JSON-line server in this driver, so a link
appears only after the real transaction rather than from an emulation.

The binary's own `version --json` values are read at runtime and become the
served health identity, so the fixture cannot drift from the binary. Four
scenarios run setup-link.sh:

1. coherent_identity_proceeds — matching version/revision, non-empty bundle and
   matching bundle identity reach the reprint (the arm server sees a request)
   and the setup link is printed.
2. version_mismatch_refused — a differing served version exits non-zero, prints
   no link, makes zero arm requests and names the version disagreement.
3. missing_bundle_refused — an omitted bundle hash exits non-zero, prints no
   link, makes zero arm requests and names the missing bundle.
4. bundle_mismatch_refused — a differing bundle version exits non-zero, prints
   no link, makes zero arm requests and names the bundle disagreement.

The four booleans are written to <log>/queries.json. The driver exits 0 once
the scenarios ran and 2 on any harness error.
"""
import json
import os
from pathlib import Path
import select
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import traceback

if os.environ.get("HERDR_S10B_SANDBOX") != "1":
    raise SystemExit("Refusing unsandboxed execution; use S10B/run-checks.sh")
SOURCE = Path(os.environ["HERDR_S10B_SOURCE"])
if str(SOURCE) != "/src":
    raise SystemExit("Expected the wrapper's /src source mount")
LOG = Path(os.environ["HERDR_S10B_LOG"])
OVERLAY = Path(os.environ["HERDR_S10B_OVERLAY"])
PREFIX = os.environ.get("HERDR_S10B_PREFIX") == "1"

HOLD_BIN = "/hold-bin/herdr-mobile-relay"

FIXTURE_TOKEN = "ab" * 32
FIXTURE_INSTANCE = "insts10b"
FIXTURE_RUN_ID = "runs10b"
FIXTURE_ORIGIN = "https://node.example.invalid:8443"
FIXTURE_PORT = "8443"
PRIOR_ORIGIN = "https://shared.example.test"
CHOSEN_ORIGIN = "https://chosen.example.test"
LINK_MARKER = CHOSEN_ORIGIN + "/#"

ARM_RESPONSE = {
    "ok": True,
    "run_id": FIXTURE_RUN_ID,
    "instance": FIXTURE_INSTANCE,
    "invitation_armed": True,
    "invitation_expires_at": "2027-01-01T00:00:00Z",
}

# The served health values, overwritten per scenario. Populated in main() from
# the frozen binary so a match is a real identity match.
BINARY_VERSION = ""
BINARY_REVISION = ""

COPIED_SCRIPTS = ("common.sh", "tailscale.sh", "setup-link.sh")
OVERLAID_SCRIPTS = ("common.sh", "tailscale.sh", "setup-link.sh")


class HarnessError(Exception):
    pass


def write_stub(path, text):
    path.write_text(text)
    path.chmod(0o755)


RELAY_STUB = """#!/bin/bash
cmd="${1:-}"
case "$cmd" in
    tailscale)
        printf '%s\\n' '{"origin":"https://node.example.invalid:8443","backend_state":"Running","logged_in":true,"serve_configured":true,"funnel_configured":false,"serve_route_count":1,"serve_route_owned":true,"serve_inspected":true,"exposure_complete":true}'
        exit 0
        ;;
    pairing-control)
        op=""
        while [ "$#" -gt 0 ]; do
            case "$1" in
                --operation) op="${2:-}"; shift 2 ;;
                *) shift ;;
            esac
        done
        if [ "$op" = status ]; then
            printf '%s\\n' '{"ok":true,"ready":true,"run_id":"insts10b","instance":"insts10b"}'
            exit 0
        fi
        printf '%s\\n' '{"ok":false,"error":"refused"}'
        exit 1
        ;;
    managed-state|normalize-origin|version|setup-fragment|qr)
        exec /hold-bin/herdr-mobile-relay "$@"
        ;;
    *)
        exit 91
        ;;
esac
"""

TAILSCALE_STUB = """#!/bin/bash
exit 0
"""

SYSTEMCTL_STUB = """#!/bin/bash
exit 3
"""

LAUNCHCTL_STUB = """#!/bin/bash
exit 3
"""


def curl_stub(health):
    return "#!/bin/bash\nprintf '%s\\n' " + shlex.quote(json.dumps(health)) + "\nexit 0\n"


class ArmServer:
    """A real Unix-socket JSON-line server answering arm_bootstrap requests."""

    def __init__(self, path, response):
        self.path = path
        self.response = response
        self.thread = None
        self.ready = threading.Event()
        self.requests = []
        self.error = None
        self.stop = False
        self.srv = None

    def start(self):
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()
        if not self.ready.wait(10):
            raise HarnessError("arm server did not become ready")
        if self.error:
            raise HarnessError("arm server failed: " + self.error)

    def _run(self):
        try:
            if self.path.exists() or self.path.is_socket():
                self.path.unlink()
            srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            srv.bind(str(self.path))
            srv.listen(4)
            srv.settimeout(0.2)
            self.srv = srv
            self.ready.set()
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline and not self.stop:
                try:
                    conn, _ = srv.accept()
                except socket.timeout:
                    continue
                except OSError:
                    break
                self._handle(conn)
        except Exception as error:  # pragma: no cover - reported as harness error
            self.error = repr(error)
            self.ready.set()
        finally:
            if self.srv is not None:
                try:
                    self.srv.close()
                except Exception:
                    pass

    def _handle(self, conn):
        try:
            conn.settimeout(10)
            data = b""
            while b"\n" not in data:
                chunk = conn.recv(4096)
                if not chunk:
                    break
                data += chunk
            try:
                self.requests.append(json.loads(data.decode("utf-8").strip()))
            except Exception:
                self.requests.append(None)
            conn.sendall((json.dumps(self.response) + "\n").encode("utf-8"))
        finally:
            conn.close()

    def arm_requests(self):
        return [request for request in self.requests
                if isinstance(request, dict) and request.get("op") == "arm_bootstrap"]

    def finish(self, timeout=6):
        self.stop = True
        if self.srv is not None:
            try:
                self.srv.close()
            except Exception:
                pass
        if self.thread is not None:
            self.thread.join(timeout)


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
    if PREFIX:
        for name in OVERLAID_SCRIPTS:
            shutil.copy2(OVERLAY / name, relay / name)
    config.chmod(0o700)
    # The product script prepends its own system PATH entries ahead of the
    # caller's PATH, so the adapters are shadowed into $HOME/.local/bin, which
    # the script places before /usr/bin. The fixture bin directory keeps the
    # canonical copy referenced directly by HERDR_RELAY_BIN/HERDR_TAILSCALE_BIN.
    shadow_dir = home / ".local" / "bin"
    shadow_dir.mkdir(parents=True)
    stubs = {
        "herdr-mobile-relay": RELAY_STUB,
        "tailscale": TAILSCALE_STUB,
        "systemctl": SYSTEMCTL_STUB,
        "launchctl": LAUNCHCTL_STUB,
    }
    for name, text in stubs.items():
        write_stub(bin_dir / name, text)
        write_stub(shadow_dir / name, text)
    return {
        "root": root,
        "relay": relay,
        "bin": bin_dir,
        "config": config,
        "home": home,
        "xdg": xdg,
        "tmp": tmp,
        "socket": config / "tailscale-control.sock",
    }


def write_private(path, text):
    path.write_text(text)
    path.chmod(0o600)


def reset_state(fx):
    config = fx["config"]
    for name in ("owner.lock", "txn.lock", "journal.json", "generation", "tailscale-control.sock"):
        path = config / name
        if path.is_dir() and not path.is_symlink():
            shutil.rmtree(path)
        elif path.exists() or path.is_symlink():
            path.unlink()
    for stage in config.glob("journal.stage.*"):
        stage.unlink()
    write_private(config / "relay.env",
                  "HERDR_RELAY_TOKEN=" + FIXTURE_TOKEN + "\n"
                  "HERDR_RELAY_INSTANCE_ID=" + FIXTURE_INSTANCE + "\n"
                  "HERDR_RELAY_TRANSPORT=tailscale\n"
                  "HERDR_TAILSCALE_ORIGIN=" + FIXTURE_ORIGIN + "\n"
                  "HERDR_TAILSCALE_HTTPS_PORT=" + FIXTURE_PORT + "\n")
    write_private(config / "tailscale-session.env",
                  "HERDR_RELAY_RUN_ID=" + FIXTURE_RUN_ID + "\n"
                  "HERDR_RELAY_PAIRING_SOCKET=" + str(fx["socket"]) + "\n"
                  "HERDR_TAILSCALE_ORIGIN=" + FIXTURE_ORIGIN + "\n"
                  "HERDR_TAILSCALE_HTTPS_PORT=" + FIXTURE_PORT + "\n")
    write_private(config / "phone-app-origin-configured", PRIOR_ORIGIN + "\n")


def install_health(fx, health):
    text = curl_stub(health)
    write_stub(fx["bin"] / "curl", text)
    write_stub(fx["home"] / ".local" / "bin" / "curl", text)


def build_health(kind):
    health = {
        "instance": FIXTURE_INSTANCE,
        "managed_run_id": FIXTURE_RUN_ID,
        "transport": "tailscale",
        "tailscale_origin": FIXTURE_ORIGIN,
        "version": BINARY_VERSION,
        "revision": BINARY_REVISION,
        "bundle_hash": "s10bbundlehash0123456789",
        "bundle_version": BINARY_VERSION,
        "bundle_revision": BINARY_REVISION,
    }
    if kind == "version_mismatch":
        health["version"] = BINARY_VERSION + "-other"
    elif kind == "missing_bundle":
        del health["bundle_hash"]
        del health["bundle_version"]
        del health["bundle_revision"]
    elif kind == "bundle_mismatch":
        health["bundle_version"] = BINARY_VERSION + "-other"
    return health


def script_env(fx):
    return {
        "HOME": str(fx["home"]),
        "XDG_CONFIG_HOME": str(fx["xdg"]),
        "TMPDIR": str(fx["tmp"]),
        "PATH": str(fx["bin"]) + ":/usr/bin:/bin",
        "HERDR_PLUGIN_CONFIG_DIR": str(fx["config"]),
        "HERDR_RELAY_BIN": str(fx["bin"] / "herdr-mobile-relay"),
        "HERDR_TAILSCALE_BIN": str(fx["bin"] / "tailscale"),
        "HERDR_PHONE_APP_URL": CHOSEN_ORIGIN,
        "NO_COLOR": "1",
        "TERM": "dumb",
    }


def start_holder(fx, env):
    holder = subprocess.Popen(
        [HOLD_BIN, "managed-state", "hold",
         "--dir", str(fx["config"]), "--operation", "owner"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        cwd=str(fx["root"]),
    )
    ready, _, _ = select.select([holder.stdout], [], [], 15)
    if not ready:
        holder.kill()
        holder.wait()
        raise HarnessError("owner holder never reported readiness")
    line = holder.stdout.readline()
    if b'"ok":true' not in line:
        holder.kill()
        holder.wait()
        raise HarnessError("owner holder readiness line: " + repr(line))
    return holder


def stop_holder(holder):
    holder.send_signal(signal.SIGTERM)
    try:
        holder.wait(timeout=15)
    except subprocess.TimeoutExpired:
        holder.kill()
        holder.wait()
    if holder.stdout is not None:
        holder.stdout.close()
    if holder.stderr is not None:
        holder.stderr.close()


def run_script(fx, env, timeout=40):
    try:
        proc = subprocess.run(
            ["/bin/bash", str(fx["relay"] / "setup-link.sh")],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            cwd=str(fx["root"]),
            timeout=timeout,
        )
        return proc.returncode, proc.stdout.decode("utf-8", "replace"), proc.stderr.decode("utf-8", "replace")
    except subprocess.TimeoutExpired as error:
        out = (error.stdout or b"").decode("utf-8", "replace")
        err = (error.stderr or b"").decode("utf-8", "replace")
        return None, out, err


def run_scenario(fx, kind):
    reset_state(fx)
    install_health(fx, build_health(kind))
    env = script_env(fx)
    holder = start_holder(fx, env)
    server = ArmServer(fx["socket"], ARM_RESPONSE)
    try:
        server.start()
        code, out, err = run_script(fx, env)
        server.finish()
        return {
            "kind": kind,
            "code": code,
            "stdout": out,
            "stderr": err,
            "combined": out + err,
            "requests": list(server.requests),
            "armRequests": server.arm_requests(),
            "serverError": server.error,
        }
    finally:
        server.finish()
        stop_holder(holder)


def scenario_ok(kind, detail):
    combined = detail["combined"]
    link_seen = LINK_MARKER in combined
    armed = bool(detail["armRequests"])
    refused = detail["code"] is not None and detail["code"] != 0
    if not link_seen and not refused and detail["code"] == 0:
        # A coherent run that never printed a link is a harness contradiction.
        return False
    if kind == "coherent_identity_proceeds":
        return bool(detail["code"] == 0 and link_seen and armed)
    if kind == "version_mismatch_refused":
        return bool(refused and not link_seen and not armed
                    and "running binary version" in combined)
    if kind == "missing_bundle_refused":
        return bool(refused and not link_seen and not armed
                    and "no managed web bundle" in combined)
    if kind == "bundle_mismatch_refused":
        return bool(refused and not link_seen and not armed
                    and "web bundle version" in combined)
    return False


def read_binary_identity():
    proc = subprocess.run([HOLD_BIN, "version", "--json"], capture_output=True, text=True, timeout=20)
    if proc.returncode != 0:
        raise HarnessError("frozen binary version --json failed: " + proc.stderr)
    identity = json.loads(proc.stdout)
    version = identity.get("version", "")
    revision = identity.get("revision", "")
    if not version or not revision:
        raise HarnessError("frozen binary reported an empty version or revision")
    return version, revision


def main():
    global BINARY_VERSION, BINARY_REVISION
    for name in COPIED_SCRIPTS:
        if not (SOURCE / "relay" / name).is_file():
            print("harness error: missing source relay/" + name, file=sys.stderr)
            return 2
    if PREFIX:
        for name in OVERLAID_SCRIPTS:
            if not (OVERLAY / name).is_file():
                print("harness error: missing pre-fix overlay " + name, file=sys.stderr)
                return 2
    try:
        BINARY_VERSION, BINARY_REVISION = read_binary_identity()
    except Exception as error:
        print("harness error: cannot read frozen binary identity: " + str(error), file=sys.stderr)
        return 2
    try:
        fx = setup_fixture()
    except Exception as error:
        print("harness error: fixture setup failed: " + str(error), file=sys.stderr)
        return 2

    scenarios = ("coherent_identity_proceeds", "version_mismatch_refused",
                 "missing_bundle_refused", "bundle_mismatch_refused")
    kinds = ("coherent", "version_mismatch", "missing_bundle", "bundle_mismatch")
    report = {}
    detail = {}
    try:
        for name, kind in zip(scenarios, kinds):
            outcome = run_scenario(fx, kind)
            report[name] = scenario_ok(name, outcome)
            detail[name] = outcome
    except Exception:
        traceback.print_exc()
        print("harness error: scenario execution failed", file=sys.stderr)
        return 2

    (LOG / "queries.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({
        "sandbox": "S10B",
        "regression": PREFIX,
        "binaryVersion": BINARY_VERSION,
        "binaryRevision": BINARY_REVISION,
        "report": report,
        "detail": detail,
    }, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

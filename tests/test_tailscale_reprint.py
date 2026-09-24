"""S7B2 reprint-wiring driver: setup-link prints a link only after a commit.

Run only through S7B2/run-checks.sh. The HERDR_S7B2_SANDBOX guard prevents
accidental host execution; bwrap, not that guard, supplies filesystem, PID and
network isolation. The product script (`relay/setup-link.sh`) is an exact copy
of the candidate (replaced by the pre-fix overlay in regression mode), and it
drives the frozen post-S7B1 holder/reprint binary mounted read-only at
/hold-bin. The arm acknowledgement is answered by a real Unix-socket JSON-line
server in this driver, so success, definite rejection and ambiguity are
distinguished by the real transaction rather than by an emulation.

Three booleans are written to <log>/queries.json:

1. success_prints_link_and_commits — exit 0, the phone-setup link is printed,
   the origin file carries the chosen origin and no journal remains.
2. rejection_prints_nothing_and_restores — non-zero exit, no link, the prior
   origin is restored and no journal remains.
3. ambiguity_prints_nothing_and_retains_journal — non-zero exit, no link, the
   prior origin is untouched, a staged journal remains and the uncertainty
   guidance reached the operator.
"""
import json
import os
from pathlib import Path
import select
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import traceback

if os.environ.get("HERDR_S7B2_SANDBOX") != "1":
    raise SystemExit("Refusing unsandboxed execution; use S7B2/run-checks.sh")
SOURCE = Path(os.environ["HERDR_S7B2_SOURCE"])
if str(SOURCE) != "/src":
    raise SystemExit("Expected the wrapper's /src source mount")
LOG = Path(os.environ["HERDR_S7B2_LOG"])
OVERLAY = Path(os.environ["HERDR_S7B2_OVERLAY"])
PREFIX = os.environ.get("HERDR_S7B2_PREFIX") == "1"

HOLD_BIN = "/hold-bin/herdr-mobile-relay"

FIXTURE_TOKEN = "ab" * 32
FIXTURE_INSTANCE = "insts7b2"
FIXTURE_RUN_ID = "runs7b2"
FIXTURE_ORIGIN = "https://node.example.invalid:8443"
FIXTURE_PORT = "8443"
PRIOR_ORIGIN = "https://shared.example.test"
CHOSEN_ORIGIN = "https://chosen.example.test"
LINK_MARKER = CHOSEN_ORIGIN + "/#"
# The shell's own ambiguity guidance (relay/setup-link.sh 6-case) is the
# contract under test; the Go command's text alone must not satisfy boolean 3.
SHELL_AMBIGUITY_MARKERS = (
    "origin was not changed",
    "later reprints are blocked",
)

COPIED_SCRIPTS = ("common.sh", "setup-link.sh")


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
            printf '%s\\n' '{"ok":true,"ready":true,"run_id":"insts7b2","instance":"insts7b2"}'
            exit 0
        fi
        printf '%s\\n' '{"ok":false,"error":"refused"}'
        exit 1
        ;;
    managed-state|normalize-origin|setup-fragment|qr)
        exec /hold-bin/herdr-mobile-relay "$@"
        ;;
    *)
        exit 91
        ;;
esac
"""

CURL_STUB = """#!/bin/bash
printf '%s\\n' '{"instance":"insts7b2","managed_run_id":"runs7b2","transport":"tailscale","tailscale_origin":"https://node.example.invalid:8443"}'
exit 0
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


class ArmServer:
    """A real Unix-socket JSON-line server answering one arm_bootstrap request."""

    def __init__(self, path, response):
        self.path = path
        self.response = response
        self.thread = None
        self.ready = threading.Event()
        self.request = None
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
            srv.listen(1)
            srv.settimeout(0.2)
            self.srv = srv
            self.ready.set()
            deadline = time.monotonic() + 25
            conn = None
            while time.monotonic() < deadline and not self.stop:
                try:
                    conn, _ = srv.accept()
                    break
                except socket.timeout:
                    continue
                except OSError:
                    break
            if conn is not None:
                conn.settimeout(10)
                data = b""
                while b"\n" not in data:
                    chunk = conn.recv(4096)
                    if not chunk:
                        break
                    data += chunk
                try:
                    self.request = json.loads(data.decode("utf-8").strip())
                except Exception:
                    self.request = None
                conn.sendall((json.dumps(self.response) + "\n").encode("utf-8"))
                conn.close()
        except Exception as error:  # pragma: no cover - reported as harness error
            self.error = repr(error)
            self.ready.set()
        finally:
            if self.srv is not None:
                try:
                    self.srv.close()
                except Exception:
                    pass

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
        shutil.copy2(OVERLAY / "setup-link.sh", relay / "setup-link.sh")
    config.chmod(0o700)
    # The product script prepends its own system PATH entries ahead of the
    # caller's PATH, so the adapters are shadowed into $HOME/.local/bin, which
    # the script places before /usr/bin. The fixture bin directory keeps the
    # canonical copy referenced directly by HERDR_RELAY_BIN/HERDR_TAILSCALE_BIN.
    shadow_dir = home / ".local" / "bin"
    shadow_dir.mkdir(parents=True)
    stubs = {
        "herdr-mobile-relay": RELAY_STUB,
        "curl": CURL_STUB,
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


def run_scenario(fx, arm_mode):
    reset_state(fx)
    env = script_env(fx)
    holder = start_holder(fx, env)
    server = None
    try:
        if arm_mode != "absent":
            if arm_mode == "success":
                response = {"ok": True, "run_id": FIXTURE_RUN_ID, "instance": FIXTURE_INSTANCE,
                            "invitation_armed": True,
                            "invitation_expires_at": "2027-01-01T00:00:00Z"}
            else:
                response = {"ok": False, "error": "refused"}
            server = ArmServer(fx["socket"], response)
            server.start()
        code, out, err = run_script(fx, env)
        if server is not None:
            server.finish()
        return {
            "code": code,
            "stdout": out,
            "stderr": err,
            "combined": out + err,
            "request": None if server is None else server.request,
            "serverError": None if server is None else server.error,
        }
    finally:
        if server is not None:
            server.finish()
        stop_holder(holder)


def origin_text(fx):
    path = fx["config"] / "phone-app-origin-configured"
    if not path.is_file():
        return None
    return path.read_text()


def journal_present(fx):
    return (fx["config"] / "journal.json").is_file()


def journal_state(fx):
    path = fx["config"] / "journal.json"
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text()).get("state")
    except Exception:
        return "unreadable"


def has_link(text):
    return LINK_MARKER in text


def has_uncertainty(text):
    lowered = text.lower()
    return all(marker in lowered for marker in SHELL_AMBIGUITY_MARKERS)


def main():
    if not (SOURCE / "relay" / "setup-link.sh").is_file():
        print("harness error: missing source relay/setup-link.sh", file=sys.stderr)
        return 2
    if PREFIX and not (OVERLAY / "setup-link.sh").is_file():
        print("harness error: missing pre-fix overlay setup-link.sh", file=sys.stderr)
        return 2
    try:
        fx = setup_fixture()
    except Exception as error:
        print("harness error: fixture setup failed: " + str(error), file=sys.stderr)
        return 2
    detail = {}
    try:
        success_detail = run_scenario(fx, "success")
        success_origin = origin_text(fx)
        success = bool(
            success_detail["code"] == 0
            and has_link(success_detail["combined"])
            and success_origin is not None
            and CHOSEN_ORIGIN in success_origin
            and not journal_present(fx)
        )
        detail["success"] = {**success_detail, "origin": success_origin, "journalPresent": journal_present(fx)}

        rejection_detail = run_scenario(fx, "rejection")
        rejection_origin = origin_text(fx)
        rejection = bool(
            rejection_detail["code"] is not None
            and rejection_detail["code"] != 0
            and not has_link(rejection_detail["combined"])
            and rejection_origin is not None
            and PRIOR_ORIGIN in rejection_origin
            and not journal_present(fx)
        )
        detail["rejection"] = {**rejection_detail, "origin": rejection_origin, "journalPresent": journal_present(fx)}

        ambiguity_detail = run_scenario(fx, "absent")
        ambiguity_origin = origin_text(fx)
        ambiguity_state = journal_state(fx)
        ambiguity = bool(
            ambiguity_detail["code"] is not None
            and ambiguity_detail["code"] != 0
            and not has_link(ambiguity_detail["combined"])
            and ambiguity_origin is not None
            and PRIOR_ORIGIN in ambiguity_origin
            and CHOSEN_ORIGIN not in ambiguity_origin
            and ambiguity_state == "staged"
            and has_uncertainty(ambiguity_detail["combined"])
        )
        detail["ambiguity"] = {**ambiguity_detail, "origin": ambiguity_origin,
                               "journalState": ambiguity_state}
    except Exception:
        traceback.print_exc()
        print("harness error: scenario execution failed", file=sys.stderr)
        return 2

    report = {
        "success_prints_link_and_commits": success,
        "rejection_prints_nothing_and_restores": rejection,
        "ambiguity_prints_nothing_and_retains_journal": ambiguity,
    }
    (LOG / "queries.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({
        "sandbox": "S7B2",
        "regression": PREFIX,
        "report": report,
        "detail": detail,
    }, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

#!/usr/bin/env python3
"""Host-side fixtures of the native-manager lifecycle workflow (hosted disposable runners only): run (bounded; run it as the uid owning the command, root for sudo operations), tls-front, herdr-socket, cloudflared, curl (strict shim), herdr (unregister-only adapter), observe, tree, exists, table-ok, nul-free, counts, ident, resolve, stop; `--name value` options, then `--` and any argv."""
import hashlib
import http.client
import http.server
import json
import os
import re
import selectors
import signal
import socket
import ssl
import stat
import subprocess
import sys
import threading
import time

LIMIT = 65536
CURL = "/usr/bin/curl"
LAUNCHCTL, SYSTEMCTL = "/bin/launchctl", "/usr/bin/systemctl"  # absolute: a PATH stub (the simulated missing manager) can never perturb observation
REPO = "0cv/herdr-mobile-relay"


def opts(argv):
    found, i = {}, 0
    while i < len(argv) and argv[i] != "--":
        if not argv[i].startswith("--") or i + 1 >= len(argv):
            sys.exit("native lifecycle host: malformed option")
        found[argv[i][2:]] = argv[i + 1]
        i += 2
    return found, argv[i + 1:]


def note(log, *fields):
    fd = os.open(log, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    try:
        os.write(fd, (" ".join(str(f) for f in fields) + "\n").encode())
    finally:
        os.close(fd)


def sh(argv, timeout=10, limit=LIMIT):
    """(status, stdout, stderr), each decoded from the RAW bytes (UTF-8, invalid bytes replaced; no newline translation, so what is parsed is what the program wrote).
    Status 127: it could not run or timed out. Status 126: stdout or stderr was longer than `limit` BYTES (the product's bound): never more than limit + 1 bytes of either
    stream is read, the program is killed, and nothing is returned, because a truncated answer is never a complete one (every caller treats a nonzero status as unknown)."""
    try:
        proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except OSError:
        return 127, "", ""
    out, err = bytearray(), bytearray()
    buffers = {proc.stdout.fileno(): out, proc.stderr.fileno(): err}
    deadline, status, ended = time.monotonic() + timeout, None, None  # ended: 126 or 127 once this function itself ends the program
    try:
        with selectors.DefaultSelector() as pending:
            for fd in buffers:
                pending.register(fd, selectors.EVENT_READ)
            while pending.get_map() and ended is None:
                ready = pending.select(max(deadline - time.monotonic(), 0))
                if not ready and time.monotonic() >= deadline:
                    ended = 127
                for key, _ in ready:
                    data = os.read(key.fd, min(65536, limit + 1 - len(buffers[key.fd])))
                    buffers[key.fd] += data
                    if not data:
                        pending.unregister(key.fd)
                    if len(buffers[key.fd]) > limit:
                        ended = 126
        if ended is None:
            status = proc.wait(timeout=max(deadline - time.monotonic(), 0.01))
    except subprocess.TimeoutExpired:
        ended = 127
    finally:
        if proc.poll() is None:
            proc.kill()
        proc.wait()
        proc.stdout.close()
        proc.stderr.close()
    if ended is not None:
        return ended, "", ""
    return status, out.decode("utf-8", "replace"), err.decode("utf-8", "replace")[:4096]


def identity(body):
    """sha256 over the readiness identity (instance, version, revision, web hash) of a ready /readyz body, else `none`."""
    try:
        doc = json.loads(body)
        inv = doc.get("inventory") or {}
        fields = [doc.get(k) for k in ("instance", "release_version", "revision", "bundle_hash")]
        if doc.get("status") == "ready" and inv.get("state") == "ready" and all(isinstance(f, str) and f for f in fields):
            return hashlib.sha256("\n".join(fields).encode()).hexdigest()
    except (ValueError, AttributeError, TypeError):
        pass
    return "none"


def forward(port, path="/readyz"):
    """GET path from 127.0.0.1:port and return (status, body); any failure is a 502, never a fabricated body. Port 0 is probe-only."""
    if port == 0:
        return 200, b'{"probe":"ok"}'
    try:
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
        conn.request("GET", path)
        reply = conn.getresponse()
        body = reply.read(LIMIT + 1)
        return (reply.status, body) if len(body) <= LIMIT else (502, b"")
    except (OSError, http.client.HTTPException):
        return 502, b""


def serve_readyz(log, tag, port):
    class Handler(http.server.BaseHTTPRequestHandler):
        def setup(self):
            if hasattr(self.request, "do_handshake"):
                self.request.do_handshake()
            super().setup()

        def log_message(self, *args):
            pass

        def do_GET(self):
            status, body = forward(port) if self.path == "/readyz" else (404, b"")
            self.send_response(status)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            note(log, tag, getattr(self.request, "nlt_sni", "-") or "-", status, identity(body))
    return Handler


def serve_until_stopped(server, stop_file):
    signal.signal(signal.SIGTERM, lambda *_: threading.Thread(target=server.shutdown).start())

    def watch():
        while not (stop_file and os.path.exists(stop_file)):
            time.sleep(0.5)
        server.shutdown()
    threading.Thread(target=watch, daemon=True).start()
    server.serve_forever(poll_interval=0.2)


def tls_front(o, rest):
    uid, gid = int(o["uid"]), int(o["gid"])
    if uid <= 0 or os.geteuid() != 0:
        sys.exit("tls-front: needs root to bind 443 and a nonroot subject uid to serve as")
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.load_cert_chain(o["cert"], o["key"])
    ctx.sni_callback = lambda sock, name, _ctx: setattr(sock, "nlt_sni", name or "-")

    class Front(http.server.ThreadingHTTPServer):
        daemon_threads = True

        def get_request(self):
            conn, addr = self.socket.accept()
            conn.settimeout(5)
            return ctx.wrap_socket(conn, server_side=True, do_handshake_on_connect=False), addr

        def handle_error(self, request, client_address):
            pass
    front = Front(("127.0.0.1", 443), serve_readyz(o["log"], "front", int(o["upstream"])))
    os.setgroups([])
    os.setgid(gid)
    os.setuid(uid)
    try:
        os.setuid(0)
    except PermissionError:
        pass
    else:
        sys.exit("tls-front: privileges were not dropped")
    with open(o["run"] + "/front.pid", "w") as stream:
        stream.write(str(os.getpid()))
    serve_until_stopped(front, o["run"] + "/stop")


def cloudflared(o, rest):
    """The fake cloudflared child of the real service wrapper: `tunnel --config FILE run`. It forwards the installed relay's real /readyz."""
    if len(rest) != 4 or rest[0] != "tunnel" or rest[1] != "--config" or rest[3] != "run":
        sys.exit("cloudflared: unsupported arguments")
    with open(rest[2], encoding="utf-8") as stream:
        conf = {}
        for key, value in re.findall(r"^\s*-?\s*([a-z-]+):\s*(\S+)", stream.read(), re.M):
            conf.setdefault(key, value)
    match = re.fullmatch(r"http://127\.0\.0\.1:([0-9]{2,5})", conf.get("service", ""))
    if not match or not conf.get("fake-listen", "").isdigit():
        sys.exit("cloudflared: config must name a loopback service and fake-listen")

    class Child(http.server.ThreadingHTTPServer):
        daemon_threads = True
        allow_reuse_address = True
    child = Child(("127.0.0.1", int(conf["fake-listen"])), serve_readyz(o["log"], "child", int(match[1])))
    signal.signal(signal.SIGTERM, lambda *_: threading.Thread(target=child.shutdown).start())
    child.serve_forever(poll_interval=0.2)


def herdr_socket(o, rest):
    """Nonroot Herdr API fixture: exactly the four empty inventory methods the relay polls; everything else is an error reply."""
    if os.geteuid() == 0:
        sys.exit("herdr-socket: refusing to run as root")
    results = {"agent.list": ("agent_list", "agents"), "workspace.list": ("workspace_list", "workspaces"),
               "tab.list": ("tab_list", "tabs"), "pane.list": ("pane_list", "panes")}
    path = o["path"]
    if os.path.exists(path) or os.path.islink(path):
        os.unlink(path)
    os.umask(0o177)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(path)
    server.listen(16)
    server.settimeout(0.5)

    def answer(conn):
        try:
            conn.settimeout(5)
            data = b""
            while b"\n" not in data and len(data) <= LIMIT:
                chunk = conn.recv(4096)
                if not chunk:
                    return
                data += chunk
            try:
                req = json.loads(data.split(b"\n", 1)[0])
                method, rid = req["method"], req["id"]
            except (ValueError, KeyError, TypeError):
                reply = {"id": "", "error": {"code": "invalid_request", "message": "invalid request: bad frame"}}
            else:
                note(o["log"], "socket", method if method in results else "refused")
                if method in results:
                    kind, key = results[method]
                    reply = {"id": rid, "result": {"type": kind, key: []}}
                else:
                    reply = {"id": rid, "error": {"code": "unknown_method", "message": "not served by the lifecycle fixture"}}
            conn.sendall(json.dumps(reply).encode() + b"\n")
        except OSError:
            pass
        finally:
            conn.close()
    with open(o["run"] + "/socket.pid", "w") as stream:
        stream.write(str(os.getpid()))
    stop = [False]
    signal.signal(signal.SIGTERM, lambda *_: stop.__setitem__(0, True))
    while not stop[0] and not os.path.exists(o["run"] + "/stop"):
        try:
            conn, _ = server.accept()
        except socket.timeout:
            continue
        threading.Thread(target=answer, args=(conn,), daemon=True).start()
    os.unlink(path)


def refuse(o, why):
    if o.get("log"):
        note(o["log"], o.get("tag", "curl"), "refused", why)
    print("native lifecycle host: refused (%s)" % why, file=sys.stderr)
    sys.exit(2)


def curl_shim(o, rest):
    """Strict curl: only the product's own flags; three exact download URLs served from the checksum-bound artifact; loopback
    /healthz and /readyz plus exactly https://<chosen host>/readyz delegated to the real curl with no config, proxy or override."""
    version, host, sha, target = o["version"], o["host"], o["sha"], o["target"]
    ports = set(o["ports"].split(","))
    plain, url, out, follow = {"--disable", "--fail", "--show-error", "--silent", "--location"}, None, None, False
    i = 0
    while i < len(rest):
        tok = rest[i]
        if tok in plain or re.fullmatch(r"-[fsSL]+", tok):
            follow = follow or tok == "--location" or ("L" in tok and not tok.startswith("--"))
            i += 1
        elif tok in ("--connect-timeout", "--max-time", "--max-filesize") and i + 1 < len(rest) and re.fullmatch(r"[0-9.]+", rest[i + 1]):
            i += 2
        elif tok in ("--proto", "--proto-redir") and i + 1 < len(rest) and rest[i + 1] == "=https":
            i += 2
        elif tok == "--noproxy" and i + 1 < len(rest) and rest[i + 1] == "*":
            i += 2
        elif tok in ("-H", "--header") and i + 1 < len(rest) and re.fullmatch(r"Accept: [A-Za-z0-9/+.;= -]+", rest[i + 1]):
            i += 2
        elif tok in ("--output", "-o") and i + 1 < len(rest) and out is None:
            out, i = rest[i + 1], i + 2
        elif not tok.startswith("-") and url is None:
            url, i = tok, i + 1
        else:
            refuse(o, "argument:" + (tok if re.fullmatch(r"-{1,2}[A-Za-z][A-Za-z0-9-]{0,30}", tok) else "other"))  # flag names only, never a value
    if url is None:
        refuse(o, "no-url")
    base = "https://github.com/%s/releases/download/v%s" % (REPO, version)
    archive = "herdr-mobile-relay_%s_%s.tar.gz" % (version, target)
    commit = "https://api.github.com/repos/%s/commits/v%s" % (REPO, version)
    if url in (commit, base + "/checksums.txt", base + "/" + archive):
        checks = open(o["art"] + "/checksums.txt", "rb").read()
        payload = json.dumps({"sha": sha}).encode() if url == commit else checks
        if url.endswith(archive):
            payload = open(o["art"] + "/" + archive, "rb").read()
            want = "%s  %s\n" % (hashlib.sha256(payload).hexdigest(), archive)
            if want.encode() not in checks:
                refuse(o, "archive-not-checksum-bound")
        if out in (None, "-"):
            sys.stdout.buffer.write(payload)
        else:
            with open(out, "wb") as stream:
                stream.write(payload)
        note(o["log"], "curl", "download", hashlib.sha256(payload).hexdigest()[:12])
        return 0
    local = re.fullmatch(r"http://127\.0\.0\.1:([0-9]+)/(?:healthz|readyz)", url)
    if not (local and local[1] in ports) and url != "https://%s/readyz" % host:
        refuse(o, "url")
    if follow:  # a delegated call would let an allowed URL redirect the real curl anywhere; downloads above never reach the network
        refuse(o, "redirect-following")
    args = [CURL, "--disable", "--noproxy", "*"] + [t for t in rest if t != "--disable"]
    done = subprocess.run(args, capture_output=True, env={"PATH": "/usr/bin:/bin", "LC_ALL": "C"}, timeout=130)
    sys.stdout.buffer.write(done.stdout)
    sys.stderr.buffer.write(done.stderr)
    if not local and url.endswith("/readyz"):
        note(o["log"], "curl", "public", identity(done.stdout) if done.returncode == 0 else "failed")
    return done.returncode


def herdr_adapter(o, rest):
    """Q1 stand-in for Herdr's CLI: it accepts only the full uninstall's own unregister call. It proves nothing about real registration."""
    if rest == ["plugin", "uninstall", "herdr-mobile-relay.events"]:
        note(o["log"], "adapter", "unregister")
        return 0
    refuse(dict(o, tag="adapter"), "command")


def group_gone(pgid, polls=20):
    """True only when signal 0 to the group answers ESRCH within the polling window (2 s by default)."""
    for _ in range(polls):
        try:
            os.killpg(pgid, 0)
        except OSError as error:
            # ESRCH: no member is left. EPERM is also macOS's answer for a zombie that is not yet reaped, so only a whole window
            # of it (or a live member) means an unsignalable survivor.
            if isinstance(error, ProcessLookupError):
                return True
        time.sleep(0.1)
    return False


def run_bounded(o, rest):
    """Run argv in its own session for at most --timeout seconds. Status: the command's own; 124 = timed out and the whole process group
    is confirmed gone; 125 = timed out and a member could not be stopped or signalled (never reported as a plain timeout)."""
    proc = subprocess.Popen(rest, start_new_session=True)
    try:
        code = proc.wait(timeout=int(o["timeout"]))
    except subprocess.TimeoutExpired:
        for sig in (signal.SIGTERM, signal.SIGKILL):  # TERM, then KILL for every member of the group even when its leader already exited
            try:
                os.killpg(proc.pid, sig)
                proc.wait(timeout=5)
            except (ProcessLookupError, PermissionError, subprocess.TimeoutExpired):
                pass
        gone = group_gone(proc.pid)  # only a group with no member left (ESRCH) is "timed out"; a survivor or an unsignalable member is 125
        print("native lifecycle host: command timed out" + ("" if gone else " and its process group could not be stopped"), file=sys.stderr)
        return 124 if gone else 125
    return code if code >= 0 else 128 - code


def raise_error(error):
    raise error


def tree(o, rest):
    """Digest of the bytes, modes and symlink targets of everything under the given paths: a refusal must change none of them.

    A root is skipped only when it definitely does not exist (ENOENT/ENOTDIR). Any directory that cannot be scanned and any entry that cannot be read raises,
    so the command exits nonzero and prints no digest: an unobserved subtree is never a digest of "nothing"."""
    digest = hashlib.sha256()
    for root in o["paths"].split(","):
        try:
            os.lstat(root)
        except (FileNotFoundError, NotADirectoryError):
            names = []
        else:
            names = [root] + [os.path.join(b, n) for b, d, f in os.walk(root, onerror=raise_error) for n in d + f]
        for path in sorted(names):
            mode = os.lstat(path).st_mode
            body = os.readlink(path) if stat.S_ISLNK(mode) else hashlib.sha256(open(path, "rb").read()).hexdigest() if stat.S_ISREG(mode) else ""
            digest.update(("%s %o %s\n" % (path, mode, body)).encode())
    print(digest.hexdigest())
    return 0


def tree_digest(pid):
    """Digest of the manager's main process and every descendant (pid, parent, start time, executable).

    `none` only when the pid is known to be 0, or is absent from a process table whose rows all parsed. `unknown` when the pid itself is unknown
    (None), when `ps` failed or timed out, or when a row of the manager or of one of its descendants cannot be parsed: an unobserved process is
    never "no process"."""
    if pid is None:
        return "unknown"
    if pid <= 0:
        return "none"
    code, listing, _ = sh(["ps", "-axo", "pid=,ppid=,lstart=,command="], limit=1 << 24)
    if code != 0:  # a failed, timed-out or truncated process listing is unknown, never "no process"
        return "unknown"
    parents, rows = {}, {}
    for line in listing.splitlines():
        head = re.match(r"\s*(\d+)\s+(\d+)\s", line)  # a row starts with pid and ppid; any other line is the tail of a multi-line command
        if not head:
            continue
        parents[int(head[1])] = int(head[2])
        m = re.match(r"\s*(\d+)\s+(\d+)\s+(\w{3}\s+\w{3}\s+\d+\s+[\d:]+\s+\d{4})\s+(\S+)", line)
        if m:
            rows[int(m[1])] = (int(m[2]), m[3], m[4])
    if pid not in parents:
        # The table never named the pid, but a garbled or truncated row would look the same: ask for that pid alone. Only a definite "no such process"
        # (exit 1 and no output) is none; a process the table parse missed, or an unanswered query, is unknown.
        code, answer, _ = sh(["ps", "-p", str(pid), "-o", "pid="])
        return "none" if code == 1 and not answer.strip() else "unknown"
    owned = {pid}
    while True:
        more = {p for p, parent in parents.items() if parent in owned and p not in owned}
        if not more:
            break
        owned |= more
    if not owned <= set(rows):  # an owned process whose row did not parse is unobserved, not absent
        return "unknown"
    return hashlib.sha256("\n".join("%d %d %s %s" % ((p, rows[p][0], rows[p][1], rows[p][2])) for p in sorted(owned)).encode()).hexdigest()


def file_state(path):
    """(sha256, mode) of a file; ("absent", "absent") only when lstat says ENOENT/ENOTDIR; ("unknown", "unknown") for any other failure (denied, a directory, a dangling link)."""
    try:
        os.lstat(path)
    except (FileNotFoundError, NotADirectoryError):
        return "absent", "absent"
    except OSError:
        return "unknown", "unknown"
    try:
        with open(path, "rb") as stream:
            body = stream.read()
        return hashlib.sha256(body).hexdigest(), format(os.stat(path).st_mode & 0o7777, "04o")
    except OSError:
        return "unknown", "unknown"


def framed_table(text):
    """The product's admission of a launchctl disabled table (native-install-transaction.sh), after its size bound: no NUL, and the text, trailing newlines dropped, starts with
    `disabled services = {` and ends with `}`."""
    body = text.rstrip("\n")
    return "\x00" not in text and body.startswith("disabled services = {") and body.endswith("}")


def table_ok(o, rest):
    """Exit 0 only for a file the product would admit as a disabled table: at most LIMIT bytes (read with one byte of margin) and framed_table; nothing is printed."""
    with open(o["path"], "rb") as stream:
        raw = stream.read(LIMIT + 1)
    return 0 if len(raw) <= LIMIT and framed_table(raw.decode("utf-8", "replace")) else 1


def nul_free(o, rest):
    """Exit 0 only for a file that holds no NUL byte; nothing is printed. At most LIMIT + 1 bytes are read: the caller bounds the size itself (a longer file fails there), and the product's
    bounded reader refuses a NUL before it parses anything, where a shell substitution would drop it."""
    with open(o["path"], "rb") as stream:
        return 1 if b"\x00" in stream.read(LIMIT + 1) else 0


def lf_lines(text):
    """The records of a manager answer, split on the LF delimiter ONLY, as the product reads them (`IFS= read -r` never removes a CR). One terminating LF is not a record. `str.splitlines()`
    also splits on and drops CR, VT, FF, FS, GS, RS, NEL, LS and PS: it turned a CRLF `not-found` reply into a definite absence the product refuses to read."""
    records = text.split("\n")
    return records[:-1] if records[-1] == "" else records


def disabled_flag(code, table, label):
    """`false` (disabled) / `true` (enabled) from a COMPLETE launchctl disabled table, in the product's own grammar (native-install-transaction.sh): the text, unnormalized, starts
    with `disabled services = {` and ends with `}`, holds no NUL, and names the label at most once as `"label" => true|false` (a label the table does not list is enabled).
    `unknown` for a failed, truncated, empty or malformed answer, so such an answer is never read as enabled. Any other value on a line naming the label (`enabled`, `disabled`, `maybe`,
    trailing text) is malformed, exactly as the product's parser fails on it: no alias is translated into a known state. Only trailing newlines are dropped (the product's command
    substitution does that), and only the blanks `[:space:]` matches in front of a line, so a leading blank, a space after the closing brace or a CR makes the table unknown."""
    text = table.rstrip("\n")
    if code != 0 or not framed_table(table):
        return "unknown"
    named = [line.lstrip(" \t\n\v\f\r") for line in text.split("\n") if '"%s"' % label in line]
    flags = [re.fullmatch(r'"%s" => (true|false)' % re.escape(label), line) for line in named]
    if len(named) > 1 or not all(flags):
        return "unknown"
    return "false" if flags and flags[0][1] == "true" else "true"


def observe(o, rest):
    """One bounded snapshot of a label: definition hash/mode, manager origin, state, release revision, process digest, manager generation."""
    label, definition, uid = o["label"], o["definition"], int(os.getuid())
    snap = {"release_revision": "absent"}
    snap["definition_hash"], snap["mode"] = file_state(definition)
    try:
        manifest = json.load(open(o["release"] + "/current/release-manifest.json"))
        revision = manifest["revision"] if isinstance(manifest, dict) else None
        snap["release_revision"] = revision if isinstance(revision, str) and re.fullmatch(r"[0-9a-f]{40}", revision) else "unknown"
    except (FileNotFoundError, NotADirectoryError):
        pass  # no release is installed
    except (OSError, ValueError, KeyError, TypeError):
        snap["release_revision"] = "unknown"  # unreadable or malformed: not "no release"
    pid = 0
    if o["manager"] == "launchd":
        code, out, err = sh([LAUNCHCTL, "print", "gui/%d/%s" % (uid, label)])
        if code != 0:
            absent = ("%s%s" % (out, err)).rstrip("\n") == 'Could not find service "%s" in domain for user gui: %d' % (label, uid)  # the product drops trailing newlines only
            snap.update(active="absent" if absent else "unknown", origin="absent" if absent else "unknown")
            pid = 0 if absent else None  # an unanswered query is not "no process"
        else:
            target, body = "gui/%d/%s" % (uid, label), out.rstrip("\n")
            framed = "\x00" not in out and (body.startswith(target + " = {\n") or body == target + " = {}")  # the product's own framing check on a loaded answer
            records = lf_lines(out)
            paths = [l[len("\tpath = "):] for l in records if l.startswith("\tpath = ")]
            pids = [l[len("\tpid = "):] for l in records if l.startswith("\tpid = ")]
            snap.update(active="loaded" if framed else "unknown", origin=paths[0] if framed and len(paths) == 1 else "unknown")
            states = [l[len("\tstate = "):] for l in records if l.startswith("\tstate = ")]
            if framed and len(pids) == 1 and pids[0].isdigit():
                pid = int(pids[0])
            elif not framed or pids or states not in (["not running"], ["waiting"]):
                pid = None  # unframed output, a pid line that is not a single number, or anything but exactly one recognized non-running state (blank, unknown, "running" without a pid): never "no process"
        code, off, _ = sh([LAUNCHCTL, "print-disabled", "gui/%d" % uid])
        snap["enabled"] = disabled_flag(code, off, label)
        code, mgr, _ = sh([LAUNCHCTL, "managerpid"])
        generation = lf_lines(mgr)
        snap["manager_generation"] = generation[0] if code == 0 and len(generation) == 1 and generation[0].isdigit() else "unknown"
    else:
        props = ("LoadState", "ActiveState", "UnitFileState", "FragmentPath", "DropInPaths", "MainPID")
        code, out, _ = sh([SYSTEMCTL, "--user", "show", label] + [a for p in props for a in ("-p", p)])
        pairs = [line.partition("=")[::2] for line in lf_lines(out)]
        kv = dict(pairs)
        # every property exactly once (a repeated one is a contradiction, never "the last one wins"), and no CR or NUL anywhere: the product matches the bytes it reads, so a CRLF reply is no reply
        if code != 0 or "\r" in out or "\x00" in out or len(pairs) != len(props) or set(kv) != set(props):
            snap.update(active="unknown", enabled="unknown", origin="unknown")
            pid = None  # an unanswered query is not "no process"
        elif kv["LoadState"] == "not-found":
            # Absent only as the product's own observer reads it (relay/native-install-transaction.sh): inactive, no main pid, no unit-file state. A unit the
            # manager no longer has a file for can still be running; a contradictory not-found reply is unknown, never "no unit, no process".
            gone = (kv["ActiveState"], kv["MainPID"], kv["UnitFileState"]) == ("inactive", "0", "")
            snap.update(active="absent" if gone else "unknown", enabled="false" if gone else "unknown", origin="absent" if gone else "unknown")
            pid = 0 if gone else None
        else:
            snap["active"] = kv["ActiveState"] if kv["LoadState"] == "loaded" else "unknown"
            snap["enabled"] = {"enabled": "true", "disabled": "false"}.get(kv["UnitFileState"], "unknown")
            snap["origin"] = (kv["FragmentPath"] + ("|dropins=" + kv["DropInPaths"] if kv["DropInPaths"] else "")) or "unknown"
            pid = int(kv["MainPID"]) if kv["MainPID"].isdigit() else None
            if pid == 0 and kv["ActiveState"] in ("active", "activating", "deactivating", "reloading"):
                pid = None  # a unit the manager reports as running while naming no main pid is unobserved, not "no process"
        code, mgr, _ = sh([SYSTEMCTL, "show", "user@%d.service" % uid, "-p", "MainPID", "-p", "InvocationID"])
        pairs = [line.partition("=")[::2] for line in lf_lines(mgr)]
        kv = dict(pairs)
        if code != 0 or len(pairs) != len(kv) or not kv.get("MainPID", "").isdigit():
            snap["manager_generation"] = "unknown"
        elif kv["MainPID"] == "0":
            snap["manager_generation"] = "none"
        else:
            snap["manager_generation"] = "%s:%s" % (kv["MainPID"], kv.get("InvocationID", "")) if re.fullmatch(r"[0-9a-f]{32}", kv.get("InvocationID", "")) else "unknown"
    snap["owned_pid_digest"] = tree_digest(pid)
    if o.get("digest"):
        view = {k: v for k, v in snap.items() if not (o["digest"] == "stable" and k in ("owned_pid_digest", "manager_generation"))}
        if o.get("file"):
            view["file"] = file_state(o["file"])[0]
        # An unobserved field is not a stable one: identical unknown snapshots must not hash identically. The manager generation is not part of this evidence
        # (launchctl managerpid availability is a hosted unknown) and keeps its own `unknown` value in the hash.
        unobserved = "unknown" in [v for k, v in view.items() if k != "manager_generation"]
        print("unknown" if unobserved else hashlib.sha256(json.dumps(view, sort_keys=True).encode()).hexdigest())
    else:
        print(json.dumps(snap, sort_keys=True))
    return 0


def exists(o, rest):
    """Print `yes` when the path exists (a dangling link included), `no` only when it is definitely absent (ENOENT/ENOTDIR), else `unknown`."""
    try:
        os.lstat(o["path"])
    except (FileNotFoundError, NotADirectoryError):
        print("no")
    except OSError:
        print("unknown")
    else:
        print("yes")
    return 0


def counts(o, rest):
    tally = {"front": 0, "child": 0, "matched": 0, "agent.list": 0, "workspace.list": 0, "unregister": 0, "adapter_refused": 0, "curl_refused": 0}
    try:
        with open(o["log"], encoding="utf-8") as stream:
            lines = stream.read().splitlines()
    except FileNotFoundError:
        lines = []  # nothing has been logged yet (the first case starts before any hit)
    except (OSError, UnicodeDecodeError) as error:  # an unreadable log is unknown, never "zero events": print no tally at all
        print("native lifecycle host: cannot read the hits log (%s)" % type(error).__name__, file=sys.stderr)
        return 1
    tally["lines"] = len(lines)
    for line in lines:
        f = line.split(" ")
        if f[0] == "front" and len(f) == 4 and f[1] == o["host"] and f[2] == "200" and f[3] == o["ident"]:
            tally["front"] += 1
        elif f[0] == "child" and len(f) == 4 and f[2] == "200" and f[3] == o["ident"]:
            tally["child"] += 1
        elif f[:2] == ["curl", "public"] and f[2:] == [o["ident"]]:
            tally["matched"] += 1
        elif f[0] == "socket" and f[1] in ("agent.list", "workspace.list"):
            tally[f[1]] += 1
        elif f[:2] == ["adapter", "unregister"]:
            tally["unregister"] += 1
        elif f[:2] == ["adapter", "refused"]:
            tally["adapter_refused"] += 1
        elif f[:2] == ["curl", "refused"]:
            tally["curl_refused"] += 1
    print(json.dumps(tally, sort_keys=True))
    return 0


def stop(o, rest):
    """Stop only fixtures whose pid file names a live host.py command of THIS run directory; leave the stop marker for any straggler.
    Status 1 when a process could not be inspected (its pid file is kept): an unanswered `ps` is neither gone nor ours."""
    open(o["run"] + "/stop", "w").close()
    status = 0
    for name in ("front", "socket"):
        path = o["run"] + "/%s.pid" % name
        try:
            with open(path) as stream:
                pid = int(stream.read().strip())
        except (OSError, ValueError):
            continue
        for _ in range(50):
            code, command, _ = sh(["ps", "-ww", "-p", str(pid), "-o", "command="])  # -ww: never truncate the arguments
            if code not in (0, 1):
                status = 1
                break
            if not ("native-lifecycle-host" in command and o["run"] in command):  # exited, or the pid now belongs to something else
                os.unlink(path)
                break
            time.sleep(0.1)
        else:
            os.kill(pid, signal.SIGTERM)
            os.unlink(path)
    return status


def main(argv):
    if len(argv) < 2:
        sys.exit("usage: native-lifecycle-host.py COMMAND [--option value ...] [-- argv]")
    if argv[1] == "ident":
        print(identity(sys.stdin.read(LIMIT)))
        return 0
    if argv[1] == "resolve" and len(argv) == 3:
        print(" ".join(sorted({a[4][0] for a in socket.getaddrinfo(argv[2], 443, type=socket.SOCK_STREAM)})))
        return 0
    o, rest = opts(argv[2:])
    table = {"run": run_bounded, "tls-front": tls_front, "herdr-socket": herdr_socket, "cloudflared": cloudflared, "curl": curl_shim,
             "herdr": herdr_adapter, "observe": observe, "counts": counts, "stop": stop, "tree": tree, "exists": exists, "table-ok": table_ok, "nul-free": nul_free}
    if argv[1] not in table:
        sys.exit("native lifecycle host: unknown command")
    return table[argv[1]](o, rest) or 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

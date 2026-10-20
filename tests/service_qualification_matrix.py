#!/usr/bin/env python3
"""Pure inventory, selection and evidence helper for service qualification.

It reads tests/test_native_install.sh and tests/test_service_uninstall_cli.py as TEXT and enumerates each suite's
cells by static interpretation of its control flow (loops, conditions, f-string cell ids). Everything else is
skipped or evaluated as expressions over a whitelist of pure builtins: it never imports or runs a suite,
builds or starts anything, touches HOME, the network or a manager, and has no import-time effects.
A cell is one semantic id (native: the fixture name; uninstall CLI: manager/family/parameters, one fixture group of
one or more Fixture.run invocations, or a control); the full tier is every cell, the fast tier the explicit subset in
tests/native_install_fast_cases.json or tests/uninstall_cli_fast_cases.json. Shards, evidence and the aggregate gate
are computed from those ids only, never from an invocation counter or a line number.
"""
import argparse
import ast
import builtins
import functools
import hashlib
import json
import os
import re
import sys
from types import SimpleNamespace

SCHEMA = 1
# Fixed shard counts per tier and runner OS, sized from hosted measurements (task-l-evidence/calibration.json): the smallest count whose
# largest MODELED hosted-macOS shard (cost() balanced by assign(), plus SHARD_OVERHEAD) stays within PROJECTION_LIMIT. A modeled shard is a
# projection, never a measured hosted time; changing a count is a deliberate edit of this line, the workflows and the docs together.
SHARD_COUNTS = {"fast": 5, "full": 28}
SUITE_BOUND = 900  # seconds; the driver's unchanged SUITE_TIMEOUT_SECONDS (the suite is killed, and its shard fails closed, at this bound)
PROJECTION_LIMIT = 540.0  # seconds; 60% of SUITE_BOUND: room for run-to-run variance (hosted macOS ran one shard up to 1.4x slower than another run)
RUNNER_OS = {"macos-15": "Darwin", "ubuntu-24.04": "Linux"}
NATIVE = "tests/test_native_install.sh"
UNINSTALL_CLI = "tests/test_service_uninstall_cli.py"
# The suites job: the nine suites that run unsharded (the native install and the uninstall CLI are their own sharded jobs).
SUITES = ("tests/test_common.sh", "tests/test_service_credentials.sh", "tests/test_service_supervision.py",
          "tests/test_service_transport_dispatch.py", "tests/test_plugin_doctor.py", "tests/test_setup_menu.py",
          "tests/test_uninstall.sh", "tests/test_tailscale_cli_setup_failure.py", "tests/test_release_scripts.sh")
# The exhaustive inventory enumerated from the committed suite at 9ad13cba (baseline-inventory.json): the totals, the
# digest of the ids and the digest of every cell's own id/run/probe record (a run or probe moved between cells changes
# it). A suite edit that changes any of them must change these numbers deliberately; nothing adjusts them silently.
FROZEN = {"cells": 2331, "runs": 1864, "probes": 576, "cancels": 8,
          "digest": "85271de3a24a8fd3a9268e6e79a10eed1326ba9bbbc38b7fc5594a9dc6e5ca73",
          "cell_digest": "885eb859c6d009bc648075e36bc52225cdfbc4c1718bfdc5d10c3e19c62905e5"}
LIMIT = 4 * 1024 * 1024


class Refusal(Exception):
    """A fixed, secret-free refusal."""


def require(condition, message):
    if not condition:
        raise Refusal(message)


# ------------------------------------------------------------------ static enumeration ----

SAFE = {name: getattr(builtins, name) for name in (
    "len", "range", "str", "int", "bool", "set", "list", "tuple", "dict", "sorted", "enumerate", "zip", "bytes",
    "repr", "min", "max", "any", "all", "isinstance")}
PURE = ("capture_frames", "capture_faults")
MISSING = object()


class _Flow(Exception):
    def __init__(self, value=None):
        self.value = value


class _Return(_Flow):
    pass


class _Continue(_Flow):
    pass


class _Break(_Flow):
    pass


def suite_program(text):
    """The one quoted heredoc Python program of the suite that defines the cell functions."""
    blocks, current = [], None
    for line in text.splitlines():
        if current is None and re.search(r"<<'PY'$", line):
            current = []
        elif current is not None and line == "PY":
            blocks.append("\n".join(current))
            current = None
        elif current is not None:
            current.append(line)
    programs = [block for block in blocks if "def native_capture_cases" in block]
    require(current is None and len(programs) == 1, "the suite must hold exactly one cell program")
    return programs[0]


class Enumerator:
    """legacy=False: a cell starts at each `cell(id)` gate. legacy=True: at each `d.Fixture(parent, id, ...)`
    (the pre-gate source); gates, when present, are evaluated as always taken and record nothing. cli=True: the
    uninstall CLI, whose cells hold any number of `f.run(...)` invocations and are enumerated from main()."""

    def __init__(self, program, legacy, cli=False):
        self.tree, self.source, self.fixture, self.texts = ast.parse(program), program, None, {}
        self.legacy, self.cli = legacy, cli
        self.functions = {n.name: n for n in self.tree.body if isinstance(n, ast.FunctionDef)}
        self.cells, self.cell, self.codes, self.marks, self.sums = [], None, {}, {}, {}
        self.relevant = set()
        while True:  # functions that reach a cell, run or probe, transitively (never memoized while this grows)
            more = {name for name, node in self.functions.items() if name not in self.relevant | {"run", "probe", "cell"}
                    and any(self.is_mark(n) for n in ast.walk(node))}
            if not more:
                break
            self.relevant |= more
        self.namespace = {"__builtins__": SAFE, "os": SimpleNamespace(getuid=lambda: 0),
                          "signal": SimpleNamespace(SIGINT=2, SIGTERM=15)}
        for name in PURE:
            self.namespace[name] = lambda *a, _n=name: self.pure(_n, a)

    # Does the subtree hold a call that opens a cell or runs/probes one (or reaches a function that does)?
    def tracked(self, node):
        key = id(node)
        if key not in self.marks:
            self.marks[key] = any(self.is_mark(n) for n in ast.walk(node))
        return self.marks[key]

    def is_mark(self, n):
        if not isinstance(n, ast.Call):
            return False
        if isinstance(n.func, ast.Name):
            return n.func.id in ("run", "probe", "cell") or n.func.id in self.relevant
        return self.fixture_run(n) or self.legacy and isinstance(n.func, ast.Attribute) and n.func.attr == "Fixture"

    def text(self, node):  # the node's own source text, found once
        if id(node) not in self.texts:
            self.texts[id(node)] = ast.get_source_segment(self.source, node)
        return self.texts[id(node)]

    def invocation(self, node, frame):
        """What one `f.run(...)` runs and with which parameters: its source text, the text of the Fixture it runs on and every scalar
        in scope (the loop variables that parameterize the case). Only a digest is kept; it is part of what the inventory freezes."""
        scalars = sorted((k, repr(v)) for k, v in frame.items() if isinstance(v, (str, int, bool, tuple, type(None))) and not k.startswith("_"))
        return hashlib.sha256(repr((self.text(node), self.fixture, scalars)).encode()).hexdigest()[:16]

    def fixture_run(self, n):  # the uninstall CLI invokes its entrypoint only as `f.run(...)` (not subprocess.run)
        return self.cli and isinstance(n.func, ast.Attribute) and n.func.attr == "run" and isinstance(n.func.value, ast.Name) and n.func.value.id == "f"

    def value(self, node, frame):
        code = self.codes.get(id(node))
        if code is None:
            expression = node if isinstance(node, ast.Expression) else ast.Expression(body=node)
            code = self.codes[id(node)] = compile(ast.fix_missing_locations(expression), "<cells>", "eval")
        try:
            return eval(code, {**self.namespace, **frame})
        except Exception:
            return MISSING

    def need(self, node, frame, what):
        result = self.value(node, frame)
        require(result is not MISSING, "cannot statically resolve " + what)
        return result

    def open(self, cell_id):
        require(isinstance(cell_id, str) and cell_id, "a cell id must be a non-empty string")
        self.cell = {"id": cell_id, "runs": 0, "probes": 0}
        self.cells.append(self.cell)

    def count(self, kind):
        require(self.cell is not None, kind + " outside any cell")
        self.cell[kind] += 1

    def calls(self, node, frame):
        for child in ast.iter_child_nodes(node):
            self.calls(child, frame)
        if not isinstance(node, ast.Call):
            return
        if isinstance(node.func, ast.Name):
            if node.func.id == "run":
                self.count("runs")
            elif node.func.id == "probe":
                self.count("probes")
            elif node.func.id == "cell":
                raise Refusal("cell() is only allowed as an if gate")
        elif self.fixture_run(node):
            self.count("runs")
            self.cell.setdefault("calls", []).append(self.invocation(node, frame))
        elif self.legacy and isinstance(node.func, ast.Attribute) and node.func.attr == "Fixture":
            self.open(self.need(node.args[1], frame, "a fixture name"))

    def bind(self, target, value, frame):
        if isinstance(target, ast.Name):
            frame.pop(target.id, None) if value is MISSING else frame.__setitem__(target.id, value)
        elif isinstance(target, (ast.Tuple, ast.List)):  # nested targets bind recursively; a wrong shape or an unknown value binds nothing
            try:
                items = list(value)
                require(len(items) == len(target.elts), "shape")
            except Exception:
                items = [MISSING] * len(target.elts)
            for element, item in zip(target.elts, items):
                self.bind(element, item, frame)

    def invoke(self, call, frame):
        function = self.functions[call.func.id]
        names = [a.arg for a in function.args.args]
        inner = {}
        for name, arg in zip(names, call.args):
            value = self.value(arg, frame)
            if value is not MISSING:
                inner[name] = value
        try:
            self.block(function.body, inner)
        except _Return as done:
            return done.value
        return None

    def pure(self, name, args):
        function = self.functions[name]
        frame = dict(zip([a.arg for a in function.args.args], args))
        try:
            self.block(function.body, frame)
        except _Return as done:
            require(done.value is not MISSING, "unresolved pure function " + name)
            return done.value
        raise Refusal("pure function without a result")

    def block(self, body, frame):
        for node in body:
            self.statement(node, frame)

    def statement(self, node, frame):
        kind = type(node)
        if kind is ast.Assign:
            self.calls(node.value, frame)
            if self.cli and isinstance(node.value, ast.Call) and getattr(node.value.func, "id", "") == "Fixture":
                self.fixture = self.text(node.value)
            value = self.value(node.value, frame)
            for target in node.targets:
                self.bind(target, value, frame)
        elif kind is ast.AugAssign and isinstance(node.target, ast.Name):
            self.calls(node.value, frame)
            if id(node) not in self.sums:  # built once: the code cache is keyed by node identity
                load = ast.Name(id=node.target.id, ctx=ast.Load())
                self.sums[id(node)] = ast.BinOp(left=load, op=node.op, right=node.value)
            self.bind(node.target, self.value(self.sums[id(node)], frame), frame)
        elif kind is ast.Expr:
            call = node.value
            name = call.func.id if isinstance(call, ast.Call) and isinstance(call.func, ast.Name) else None
            if self.legacy and name == "native_shim_parity":
                self.open("shim-parity")
            elif name in self.relevant:
                self.invoke(call, frame)
            else:
                self.calls(call, frame)
        elif kind is ast.Assert:
            self.calls(node.test, frame)
        elif kind is ast.Return:
            raise _Return(self.value(node.value, frame) if node.value is not None else None)
        elif kind is ast.Continue:
            raise _Continue()
        elif kind is ast.Break:
            raise _Break()
        elif kind is ast.If:
            self.branch(node, frame)
        elif kind is ast.For:
            self.loop(node, frame)
        elif kind is ast.With:
            for item in node.items:
                self.calls(item.context_expr, frame)
                if isinstance(item.optional_vars, ast.Name):
                    frame.pop(item.optional_vars.id, None)
            self.block(node.body, frame)
        elif kind is ast.Try:
            self.block(node.body, frame)
        elif kind is ast.While:
            require(not self.tracked(node), "a while loop may not hold cells")

    def branch(self, node, frame):
        test = node.test
        negated = isinstance(test, ast.UnaryOp) and isinstance(test.op, ast.Not)
        gate = test.operand if negated else test
        if isinstance(gate, ast.Call) and isinstance(gate.func, ast.Name) and gate.func.id == "cell":
            require(len(gate.args) == 1 and not gate.keywords, "malformed cell gate")
            cell_id = self.need(gate.args[0], frame, "a cell id")
            if not self.legacy:
                self.open(cell_id)
            self.block(node.orelse if negated else node.body, frame)
            return
        verdict = self.value(test, frame)
        if verdict is MISSING:
            require(not self.tracked(node), "cannot statically resolve a condition that holds cells")
            return
        self.calls(test, frame)
        self.block(node.body if verdict else node.orelse, frame)

    def loop(self, node, frame):
        if not self.tracked(node):
            return
        require(not node.orelse, "a loop with else may not hold cells")
        for item in self.need(node.iter, frame, "a loop iterable"):
            self.bind(node.target, item, frame)
            try:
                self.block(node.body, frame)
            except _Continue:
                continue
            except _Break:
                break


def enumerate_cells(text, legacy=False, cli=False):
    """Ordered cells [{id, runs, probes}] of a suite source text; refuses anything it cannot resolve."""
    require(not (cli and legacy), "the uninstall CLI has no pre-gate enumeration")
    walker = Enumerator(text if cli else suite_program(text), legacy, cli)
    walker.block(walker.functions["main"].body if cli else walker.tree.body, {})
    ids = [cell["id"] for cell in walker.cells]
    require(len(ids) == len(set(ids)) == len({i.lower() for i in ids}), "cell ids must be unique, even on a case-insensitive volume")
    if cli:  # a control runs no entrypoint; every other cell runs it at least once
        require(all((c["runs"] == 0) == c["id"].startswith("control/") for c in walker.cells), "a cell and its entrypoint runs disagree")
    else:
        require(all(c["runs"] <= 1 for c in walker.cells), "a cell runs the installer at most once")
    return walker.cells


def digest(ids):
    return hashlib.sha256(("\n".join(sorted(ids)) + "\n").encode()).hexdigest()


def totals(cells):
    records = sorted("%s %d %d" % (c["id"], c["runs"], c["probes"]) for c in cells)  # every cell's own run and probe count
    found = {"cells": len(cells), "runs": sum(c["runs"] for c in cells), "probes": sum(c["probes"] for c in cells),
             "cancels": sum("-cancel-" in c["id"] for c in cells), "digest": digest([c["id"] for c in cells]),
             "cell_digest": hashlib.sha256(("\n".join(records) + "\n").encode()).hexdigest()}
    if any("calls" in c for c in cells):  # the uninstall CLI: every invocation's call, fixture and parameters, IN ORDER
        found["invocation_digest"] = hashlib.sha256("\n".join("%s %s" % (c["id"], d) for c in cells for d in c.get("calls", [])).encode()).hexdigest()
    return found


# ------------------------------------------------------------------ classification ----

_P, _T, _R = "(?P<platform>systemd|launchd)", "(?P<transport>gateway|cloudflare)", "(?P<role>current|legacy)"
_SEAM, _INIT = "(?P<seam>systemd|domain|label|disabled)", "(?P<initial>True|False)"
_FAULT = "(?P<fault>middle-nul|prefix-nul|suffix-nul|oversized|failed|partial|empty)"
FAILURES = ("read|query|snapshot|candidate|chmod|token|rename|commit|reload|start|stop|ready-stop-fail|ready-still-active|"
            "ready-unknown|ready-restore|ready-safe|public-stop-fail|public-unknown|public-safe|prior-unready")
PATTERNS = (
    ("parity", r"shim-parity"),
    ("probe", rf"capture-probe-{_P}-{_T}-{_R}-{_SEAM}-{_FAULT}-(?P<invocation>observe-if|observe-or|quiet-if|quiet-or)"),
    ("valid", rf"capture-valid-{_P}-{_T}-{_R}"),
    ("exit", rf"capture-exit-{_P}-{_T}-{_R}-{_SEAM}-{_INIT}-(?P<phase>initial|fresh|ready|public)-(?P<at>[1-4])-{_FAULT}-(?P<conditional>if|or|)"),
    ("origin", rf"origin-{_P}-{_R}-(?P<kind>foreign-bare|foreign-file|empty|duplicate|dropin|fail|oversized|blank|malformed-space|malformed-tab|malformed)"),
    ("origin-control", rf"origin-{_P}-(?P<control>match|fresh)"),
    ("origin-input", rf"origin-input-{_P}"),
    ("refuse", rf"{_P}-{_T}-refuse-(?P<name>run|control|pairing|rearm-invalid|rearm|conflict|expansion|routing|raw-distinct|pointer-relative)"),
    ("alias", rf"{_P}-{_T}-future-alias"),
    ("key", rf"{_P}-{_T}-key-(?P<length>5|33|32|16)"),
    ("false", rf"{_P}-{_T}-false-(?P<number>[0-5])-(?P<spelling>0|false|False|FALSE|f|F)"),
    ("identities", rf"{_P}-{_T}-empty-identities"),
    ("overlap", rf"{_P}-{_T}-pointer-overlap-(?P<pointer>environment|current|legacy)-(?P<prior>True|False)"),
    ("pointer", rf"{_P}-{_T}-pointer"),
    ("defaults", rf"{_P}-{_T}-defaults-(?P<missing>file|key|instance)"),
    ("cancel", rf"{_P}-{_T}-cancel-(?P<signal>2|15)"),
    ("matrix", rf"{_P}-{_T}-(?P<initial>active|inactive|fresh)-(?P<failure>ok|{FAILURES})"))
COMPILED = tuple((family, re.compile(pattern)) for family, pattern in PATTERNS)


def classify(cell_id):
    found = [(family, m) for family, rx in COMPILED for m in [rx.fullmatch(cell_id)] if m]
    require(len(found) == 1, "cell id matches no single family: " + cell_id)
    family, m = found[0]
    return dict(m.groupdict(), family=family)


# Modeled hosted macos-15 seconds per native cell: the bottleneck runner balances the shards and sizes their count. Calibrated in
# task-l-evidence/calibration.json (task-l-measure/calibrate.py) from the suite's own `native installation fixture invocation N (platform)
# took Xs` lines of six hosted fast-tier jobs (three runs, commits 1bb8723f 0598331f 6304e405, two shards each); no full-tier shard has
# ever run hosted. A modeled value is a PROJECTION used to balance shards and to size their count, never a measured hosted time.
#   CLASS_SECONDS   MEASURED classes, (systemd, launchd): the largest mean seconds of one timed installer invocation of the class over the
#                   six jobs, rounded up. The full tier's capture-ready and capture-public classes (1152 of 2331 cells) are priced from the
#                   24 cells of each the fast tier holds.
#   MODELED_SECONDS the classes no invocation line times (probes, validation, cancellation, parity): the older local Ubuntu-equivalent
#                   estimate (seconds, launchd multiplier) times MACOS_FACTOR, the ratio of the measured seconds above to those estimates
#                   over the timed fast-tier cells.
#   SHARD_OVERHEAD  the largest suite time outside a timed invocation (suite seconds minus the invocation sum) of those jobs, added to
#                   every shard. It already holds the real cost of the modeled classes, which cost() prices again per cell: deliberately
#                   conservative, since the measurements cannot split it into a fixed part and a per-cell part.
CLASS_SECONDS = {"capture-initial": (1.08, .5), "capture-fresh": (2.94, 1.95), "capture-ready": (6.0, 7.89), "capture-public": (7.87, 8.27),
                 "install-ok": (4.27, 3.99), "early": (1.32, .89), "quiesce": (7.86, 10.7), "public": (8.48, 10.4), "ready": (8.99, 11.12),
                 "refusal": (.33, .15), "origin-refusal": (1.35, .53)}
MODELED_SECONDS = {"probe": (.17, 1.16), "valid": (2.4, 1.3), "cancel": (4.75, 1.75), "parity": (8.0, 1.0)}
MACOS_FACTOR = 2.11
SHARD_OVERHEAD = 127.0
EARLY = ("read", "query", "snapshot", "candidate", "chmod", "token")


def cost_class(a):
    family = a["family"]
    if family == "exit":
        return "capture-" + a["phase"]
    if family == "matrix":
        failure = a["failure"]
        return ("install-ok" if failure == "ok" else "early" if failure in EARLY else
                "public" if failure.startswith("public") else "ready" if failure.startswith(("ready", "prior")) else "quiesce")
    return {"parity": "parity", "probe": "probe", "valid": "valid", "origin": "origin-refusal", "cancel": "cancel",
            "origin-input": "refusal", "refuse": "refusal", "alias": "refusal", "overlap": "refusal"}.get(family, "install-ok")


def cost(cell_id):
    a = classify(cell_id)
    name, launchd = cost_class(a), a.get("platform") == "launchd"
    if name in CLASS_SECONDS:
        return CLASS_SECONDS[name][launchd]
    seconds, multiplier = MODELED_SECONDS[name]
    return round(seconds * (multiplier if launchd else 1.0) * MACOS_FACTOR, 3)


# Uninstall CLI ids are manager/family/parameters (manager: the model, systemd or launchd) or control/name. Each family lists its
# parameters, so an id that matches no family, or has a different parameter count, is refused. The Fixture.run entrypoint cost is the
# measured process-tree CPU of Task G's complete local run of the unchanged suite (retry3-work-cli.json) per run, systemd and
# launchd, times 1.38 hosted macOS seconds per local CPU second (Task G calibration over 1062 hosted invocations): a PROJECTION used
# to balance shards, never a hosted time. The controls and the per-fixture construction are modeled the same way.
CLI_FAMILIES = {
    "present": ("tag",), "initial": ("state",), "direct": ("fault",), "opposite": (), "fault": ("index", "fault"),
    "capture": ("index", "seam", "form", "action"), "late": ("index", "count", "action"), "bound": ("size", "action"),
    "definition": ("shape",), "ancestor-absent": (), "ancestor-unsearchable": ("depth",), "route-spelling": ("spelling",),
    "route-choice": ("choice",), "route-failure": (), "route-prior": ("prior",), "usage": ("args",), "env-text": ("text",),
    "env-key": ("key", "value"), "role-selected": ("role", "index"), "role-alias": ("role",),
    "identity-alias": ("role", "index", "fault", "side"), "plist-label": ("index", "kind"), "tool-fault": ("fault",),
    "legacy-selection": (), "config-input": ("bad",), "stopped": ("action", "state"), "rollback-unauthorized": (),
    "signal": ("signal", "seam", "index"), "caller": ("fault", "caller"), "config-absent": (), "ambient-transport": (),
    "quoted-route": ("quoting",), "percent-specifier": ("quoting", "index", "form"),
    "override": ("quoting", "index", "directive", "side"), "section": ("quoting", "index", "section"),
    "overlay": ("index", "present", "location"), "metadata-field": ("index", "present", "field", "fault"),
    "metadata-fault": ("index", "fault"), "metadata-override": ("index", "override"), "fragment-bystander": ("index",),
    "cached-unit": ("index",), "absent-override": ("index", "override"), "absent-not-found": ("index",),
    "drift": ("index", "at", "field"), "meta-caller": ("fault", "caller")}
CLI_CONTROLS = {"timeout-diagnostics": 4.0, "fast-stub-equivalence": 45.0, "metadata-batch": 0.5}  # modeled seconds
CLI_FIXTURE = 0.03
CLI_RUN = {  # modeled hosted-macOS seconds per Fixture.run: (systemd model, launchd model)
    "absent-not-found": (1.25, 1.25), "absent-override": (.22, .22), "ambient-transport": (.49, .28), "ancestor-absent": (1.33, .76),
    "ancestor-unsearchable": (.06, .05), "bound": (1.41, 1.41), "cached-unit": (.17, .17), "caller": (.58, .46), "capture": (.62, .53),
    "config-absent": (1.08, .5), "config-input": (.3, .21), "definition": (.11, .09), "direct": (1.75, 1.31), "drift": (1.36, 1.36),
    "env-key": (.41, .28), "env-text": (.45, .28), "fault": (1.98, 1.72), "fragment-bystander": (.23, .23), "identity-alias": (1.13, .73),
    "initial": (1.82, 1.31), "late": (1.24, 1.13), "legacy-selection": (.66, .49), "meta-caller": (.13, .13), "metadata-fault": (.21, .21),
    "metadata-field": (.2, .2), "metadata-override": (.23, .23), "opposite": (.11, .09), "overlay": (.22, .22), "override": (.2, .2),
    "percent-specifier": (.58, .58), "plist-label": (.17, .17), "present": (1.56, 1.08), "quoted-route": (1.62, 1.62), "role-alias": (.44, .28),
    "role-selected": (.53, .33), "rollback-unauthorized": (.13, .07), "route-choice": (1.3, .88), "route-failure": (.89, .68),
    "route-prior": (1.72, 1.13), "route-spelling": (1.27, .85), "section": (.2, .2), "signal": (1.48, 1.17), "stopped": (.53, .65),
    "tool-fault": (.18, .15), "usage": (.01, .01)}


def classify_cli(cell_id):
    parts = cell_id.split("/")
    if parts[0] == "control":
        require(len(parts) == 2 and parts[1] in CLI_CONTROLS, "cell id matches no control: " + cell_id)
        return {"family": "control", "name": parts[1]}
    require(len(parts) >= 2 and parts[0] in ("systemd", "launchd") and parts[1] in CLI_FAMILIES
            and len(parts) == 2 + len(CLI_FAMILIES[parts[1]]) and all(parts), "cell id matches no family: " + cell_id)
    return dict(zip(CLI_FAMILIES[parts[1]], parts[2:]), family=parts[1], platform=parts[0])


def cost_cli(cell_id, runs=1):
    a = classify_cli(cell_id)
    if a["family"] == "control":
        return CLI_CONTROLS[a["name"]]
    return round(CLI_FIXTURE + runs * CLI_RUN[a["family"]][a["platform"] == "launchd"], 3)


class Profile:
    """One sharded suite: where its cells come from, how many shards a tier has, what it records, which cells are pinned to
    shard 1 (the uninstall CLI's controls run in exactly one shard per runner and tier) and the modeled seconds each shard adds."""

    def __init__(self, **fields):
        self.__dict__.update(fields)


NATIVE_P = Profile(name="native", source=NATIVE, fast="tests/native_install_fast_cases.json", counts=SHARD_COUNTS, cli=False,
                   frozen=FROZEN, env="HERDR_NATIVE_INSTALL", kind="native-install-shard", artifact="native",
                   evidence="native-shard-evidence.json", classify=classify, cost=lambda i, runs=1: cost(i), pin=lambda i: False,
                   overhead=SHARD_OVERHEAD)
# The uninstall CLI inventory enumerated from the gated suite: 1032 cells holding the 1657 Fixture.run invocations the unchanged 0598331f
# suite makes (checked invocation by invocation against that source in task-j-evidence/baseline-inventory.json) and its 3 controls. The
# digest of the ids and the digest of every cell's own id/run count freeze it exactly as FROZEN does for the native suite; the
# invocation digest also freezes, in order, what each of the 1657 invocations is (its call, its Fixture and its loop variables).
CLI_FROZEN = {"cells": 1032, "runs": 1657, "probes": 0, "cancels": 0,
              "digest": "3933b90d3589f14462492b8c225e8b4b33526f854daa9412f9c41ebe9927f0f1",
              "cell_digest": "b8e2a4a3a49caaff3afc7fca5119bd4f35e7beda718b0578eb16bbc64d877158",
              "invocation_digest": "712209c922ad95c7ff23d3a0fb746e11991d50b6c10441b38d3f97b4e65eb466"}
CLI = Profile(name="uninstall-cli", source=UNINSTALL_CLI, fast="tests/uninstall_cli_fast_cases.json", counts={"fast": 1, "full": 3},
              cli=True, frozen=CLI_FROZEN, env="HERDR_UNINSTALL_CLI", kind="uninstall-cli-shard", artifact="uninstall-cli",
              evidence="uninstall-cli-shard-evidence.json", classify=classify_cli, cost=cost_cli, pin=lambda i: i.startswith("control/"),
              overhead=0.0)
PROFILES = (NATIVE_P, CLI)


# ------------------------------------------------------------------ tiers, shards, plan ----

def load_fast(text, inventory):
    document = json_load(text)
    require(type(document) is dict and document.get("schema_version") == SCHEMA and isinstance(document.get("groups"), list),
            "malformed fast case file")
    ids = []
    for group in document["groups"]:
        require(type(group) is dict and set(group) == {"name", "rationale", "ids"} and group["rationale"]
                and isinstance(group["ids"], list) and all(isinstance(i, str) for i in group["ids"]), "malformed fast group")
        ids += group["ids"]
    require(len(ids) == len(set(ids)), "a fast case is listed twice")
    unknown = sorted(set(ids) - set(inventory))
    require(not unknown, "fast cases outside the inventory: " + ", ".join(unknown[:3]))
    return [i for i in inventory if i in set(ids)]


def assign(ids, count, suite=None, runs=None):
    """Longest-processing-time balancing by modeled cost; deterministic, shards disjoint and complete. A pinned cell (the
    uninstall CLI's controls) goes to shard 1 first and its cost counts toward that shard's load."""
    suite, runs = suite or NATIVE_P, runs or {}
    price = lambda i: suite.cost(i, runs.get(i, 1))
    shards, loads = [[i for i in ids if suite.pin(i)]] + [[] for _ in range(count - 1)], [0.0] * count
    loads[0] = sum(price(i) for i in shards[0])
    for cell_id in sorted((i for i in ids if not suite.pin(i)), key=lambda i: (-price(i), i)):
        target = loads.index(min(loads))
        shards[target].append(cell_id)
        loads[target] += price(cell_id)
    order = {cell_id: n for n, cell_id in enumerate(ids)}
    return [sorted(shard, key=order.get) for shard in shards], loads


def projection(ids, count, suite=None, runs=None):
    """The modeled seconds of each shard of `ids` at `count` shards: its balanced cell cost plus the suite's per-shard overhead. Native:
    hosted macOS. A projection to size the shard count, never a measured hosted time."""
    suite = suite or NATIVE_P
    return [round(load + suite.overhead, 1) for load in assign(ids, count, suite, runs)[1]]


@functools.lru_cache(maxsize=8)
def _inventory(source_text, suite):
    cells = enumerate_cells(source_text, cli=suite.cli)
    seen = totals(cells)
    require(seen == suite.frozen, "the suite inventory differs from the frozen baseline: %s != %s" % (seen, suite.frozen))
    return tuple((c["id"], c["runs"]) for c in cells)


def inventory_of(source_text, suite=None):
    return [i for i, _ in _inventory(source_text, suite or NATIVE_P)]


def runs_of(source_text, suite=None):
    return dict(_inventory(source_text, suite or NATIVE_P))


def tier_ids(tier, source_text, fast_text, suite=None):
    suite = suite or NATIVE_P
    require(tier in suite.counts, "unknown tier")
    inventory = inventory_of(source_text, suite)
    return inventory, (inventory if tier == "full" else load_fast(fast_text, inventory))


def shard_ids(tier, shard, inventory, ids, suite=None, runs=None):
    """(index, count, ids) for SHARD 'i/n' (n must be the tier's fixed count); None selects the whole tier."""
    suite = suite or NATIVE_P
    if not shard:
        return 0, 0, list(ids)
    m = re.fullmatch(r"([1-9][0-9]?)/([1-9][0-9]?)", shard)
    require(m and int(m[2]) == suite.counts[tier] and 1 <= int(m[1]) <= int(m[2]), "invalid shard for this tier")
    return int(m[1]), int(m[2]), assign(ids, int(m[2]), suite, runs)[0][int(m[1]) - 1]


def plan(source_text, fast_text, env, suite=None):
    suite = suite or NATIVE_P
    chosen = {name: env.get(suite.env + "_" + name) for name in ("TIER", "SHARD", "CELL")}  # None: not set
    require(all(value != "" for value in chosen.values()), "a selection variable that is set must not be empty (unset it for the default)")
    tier = chosen["TIER"] or "full"
    inventory, ids = tier_ids(tier, source_text, fast_text, suite)
    runs = runs_of(source_text, suite)
    index, count, selected = shard_ids(tier, chosen["SHARD"] or "", inventory, ids, suite, runs)
    if chosen["CELL"] is not None:
        require(chosen["CELL"] in selected, "unknown or unselected cell id")
        selected = [chosen["CELL"]]
    return {"schema_version": SCHEMA, "tier": tier, "shard_index": index, "shard_count": count,
            "inventory": inventory, "selected": selected, "runs": runs}


# ------------------------------------------------------------------ evidence and gate ----

def _no_duplicates(pairs):
    keys = [k for k, _ in pairs]
    require(len(keys) == len(set(keys)), "duplicate JSON key")
    return dict(pairs)


def _finite(constant):
    raise Refusal("non-finite number " + constant)


def json_load(text):
    require(len(text) <= LIMIT, "JSON document too large")
    try:
        return json.loads(text, object_pairs_hook=_no_duplicates, parse_constant=_finite)
    except ValueError as error:
        raise Refusal("malformed JSON") from error


def shard_expectation(tier, shard, sha, run_id, attempt, runner_os, source_text, fast_text, suite=None):
    suite = suite or NATIVE_P
    inventory, ids = tier_ids(tier, source_text, fast_text, suite)
    index, count, mine = shard_ids(tier, shard, inventory, ids, suite, runs_of(source_text, suite))
    require(count, "a shard is required")
    return {"schema_version": SCHEMA, "kind": suite.kind, "candidate_sha": sha, "run_id": run_id,
            "run_attempt": attempt, "tier": tier, "runner_os": runner_os, "shard_index": index, "shard_count": count,
            "inventory_digest": digest(ids), "shard_digest": digest(mine), "expected_count": len(mine), "ids": mine}


def shard_evidence(want, cells_text, suite_exit):
    cells = cells_text.split("\n")
    require(cells[-1:] == [""] or not cells_text, "the cell record is not newline terminated")
    completed = [c for c in cells if c]
    doc = {k: v for k, v in want.items() if k != "ids"}
    ok = suite_exit == 0 and sorted(completed) == sorted(want["ids"]) and len(set(completed)) == len(completed)
    return dict(doc, completed_count=len(completed), suite_exit=suite_exit, result="pass" if ok else "fail",
                completed=sorted(completed))


def check_shard(doc, want):
    keys = {"schema_version", "kind", "candidate_sha", "run_id", "run_attempt", "tier", "runner_os", "shard_index",
            "shard_count", "inventory_digest", "shard_digest", "expected_count", "completed_count", "suite_exit",
            "result", "completed"}
    require(type(doc) is dict and set(doc) == keys, "shard evidence has the wrong keys")
    for key, value in want.items():
        if key != "ids":
            require(type(doc[key]) is type(value) and doc[key] == value, "shard evidence differs from the expected " + key)
    done = doc["completed"]
    require(type(done) is list and all(type(c) is str for c in done), "malformed completed list")
    require(len(done) == len(set(done)), "duplicate completed cell")
    extra, missing = sorted(set(done) - set(want["ids"])), sorted(set(want["ids"]) - set(done))
    require(not extra, "unexpected completed cell: " + ", ".join(extra[:3]))
    require(not missing, "missing completed cell: " + ", ".join(missing[:3]))
    require(type(doc["completed_count"]) is int and doc["completed_count"] == len(done) == want["expected_count"], "wrong completed count")
    require(doc["suite_exit"] == 0 and type(doc["suite_exit"]) is int and doc["result"] == "pass", "the shard did not pass")


def check_summary(doc, mode, names, runner_os, sha, run_id, attempt):
    """A summary exactly as the driver writes it: every field and nested field present, typed and (where it binds) matching."""
    keys = {"schema_version", "candidate_sha", "mode", "run_id", "run_attempt", "runner", "bash_version",
            "python_version", "suites", "checkout_unchanged", "result"}
    require(type(doc) is dict and set(doc) == keys and type(doc["schema_version"]) is int and doc["schema_version"] == 2,
            "summary has the wrong keys")
    text = lambda value: type(value) is str and value != ""
    runner = doc["runner"]
    require(type(runner) is dict and set(runner) == {"os", "arch"} and all(text(v) for v in runner.values()), "malformed runner metadata")
    require(all(text(doc[k]) for k in ("candidate_sha", "mode", "run_id", "run_attempt", "bash_version", "python_version")),
            "malformed summary metadata")
    require(doc["candidate_sha"] == sha and doc["mode"] == mode and doc["run_id"] == run_id and
            doc["run_attempt"] == attempt and runner["os"] == runner_os, "summary is stale or from another runner")
    suites = doc["suites"]
    require(type(suites) is list and all(type(s) is dict and set(s) == {"name", "exit", "seconds"} and text(s["name"]) and
                                         type(s["exit"]) is int and type(s["seconds"]) is int and s["seconds"] >= 0 for s in suites),
            "malformed suite records")
    require(sorted(s["name"] for s in suites) == sorted(names), "suite records are missing, duplicated or unexpected")
    require(all(s["exit"] == 0 for s in suites) and doc["checkout_unchanged"] is True and doc["result"] == "pass", "a suite failed")


def check_needs(text, names):
    needs = json_load(text)
    require(type(needs) is dict and set(needs) == set(names), "unexpected dependency set")
    bad = sorted(n for n in names if type(needs[n]) is not dict or needs[n].get("result") != "success")
    require(not bad, "dependency did not succeed: " + ", ".join(bad))


def read_artifact(directory, name, files):
    path = os.path.join(directory, name)
    require(os.path.isdir(path) and not os.path.islink(path), "missing artifact " + name)
    require(sorted(os.listdir(path)) == sorted(files), "artifact " + name + " has the wrong files")
    out = {}
    for file in files:
        target = os.path.join(path, file)
        require(os.path.isfile(target) and not os.path.islink(target) and 0 < os.path.getsize(target) <= LIMIT,
                "empty or unsafe artifact file " + name + "/" + file)
        with open(target, encoding="utf-8") as stream:
            out[file] = json_load(stream.read())
    return out


def gate(tier, sha, run_id, attempt, directory, texts):
    """Every shard of every sharded suite on every runner, plus the unsharded suites, must be present, current and complete.
    texts maps a profile name to its (source text, fast-cases text)."""
    require(re.fullmatch(r"[0-9a-f]{40}", sha) and run_id.isdigit() and attempt.isdigit(), "malformed run identity")
    names, wanted = {}, {}
    for runner, runner_os in RUNNER_OS.items():
        names["service-suites-%s-%s-%s" % (tier, runner, sha)] = (None, runner_os, None)
        for suite in PROFILES:
            for shard in range(1, suite.counts[tier] + 1):
                names["service-%s-%s-%s-s%d-%s" % (suite.artifact, tier, runner, shard, sha)] = (suite, runner_os, "%d/%d" % (shard, suite.counts[tier]))
    require(os.path.isdir(directory) and sorted(os.listdir(directory)) == sorted(names), "artifact set differs from the expected shards")
    for name, (suite, runner_os, spec) in sorted(names.items(), key=lambda item: item[0]):
        if suite is None:
            check_summary(read_artifact(directory, name, ["summary.json"])["summary.json"], "suites", SUITES, runner_os, sha, run_id, attempt)
            continue
        want = shard_expectation(tier, spec, sha, run_id, attempt, runner_os, *texts[suite.name], suite)
        files = read_artifact(directory, name, [suite.evidence, "summary.json"])
        doc = files[suite.evidence]
        check_shard(doc, want)  # exactly this attempt: an artifact kept from an earlier attempt of the run is stale
        check_summary(files["summary.json"], suite.name, ["%s#%s-%dof%d" % (suite.source, tier, want["shard_index"], want["shard_count"])],
                      runner_os, sha, run_id, attempt)
        wanted.setdefault((suite, runner_os), []).extend(doc["completed"])
    for (suite, runner_os), completed in wanted.items():  # exactly once per runner: every cell, the controls included, in the shard that owns it
        require(sorted(completed) == sorted(tier_ids(tier, *texts[suite.name], suite)[1]), "the %s %s shards do not cover the tier exactly once" % (suite.name, runner_os))
        kinds = {suite.classify(c)["platform"] for c in completed if "platform" in suite.classify(c)}
        require(kinds == {"systemd", "launchd"}, "a runner lacks the systemd or launchd inventory")


# ------------------------------------------------------------------ command line ----

def read(path):
    with open(path, encoding="utf-8") as stream:
        return stream.read(LIMIT + 1)


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("plan", "list", "evidence", "gate"):
        p = sub.add_parser(name)
        if name != "gate":
            p.add_argument("--suite", choices=[suite.name for suite in PROFILES], default="native")
            p.add_argument("--source")
            p.add_argument("--fast-cases")
        else:  # the gate reads every sharded suite
            for suite in PROFILES:
                p.add_argument("--%s-source" % suite.name, default=suite.source)
                p.add_argument("--%s-fast-cases" % suite.name, default=suite.fast)
        if name == "list":
            p.add_argument("--legacy", action="store_true")
            p.add_argument("--tier", default="full")
        if name == "plan":
            p.add_argument("--output", required=True)
        if name in ("evidence", "gate"):
            p.add_argument("--tier", required=True)
            p.add_argument("--sha", required=True)
            p.add_argument("--run-id", required=True)
            p.add_argument("--run-attempt", required=True)
        if name == "evidence":
            p.add_argument("--shard", required=True)
            p.add_argument("--runner-os", required=True)
            p.add_argument("--suite-exit", type=int, required=True)
            p.add_argument("--cells", required=True)
            p.add_argument("--output", required=True)
        if name == "gate":
            p.add_argument("--artifacts", required=True)
    sub.add_parser("needs").add_argument("names", nargs="+")
    args = parser.parse_args(argv)
    try:
        if args.command == "needs":
            check_needs(os.environ.get("HERDR_NEEDS_JSON", ""), args.names)
        elif args.command == "gate":
            texts = {suite.name: (read(getattr(args, "%s_source" % suite.name.replace("-", "_"))),
                                  read(getattr(args, "%s_fast_cases" % suite.name.replace("-", "_")))) for suite in PROFILES}
            gate(args.tier, args.sha, args.run_id, args.run_attempt, args.artifacts, texts)
        else:
            suite = next(suite for suite in PROFILES if suite.name == args.suite)
            source, fast = read(args.source or suite.source), lambda: read(args.fast_cases or suite.fast)
        if args.command == "list":
            cells = enumerate_cells(source, args.legacy, suite.cli)
            inventory = [c["id"] for c in cells]
            ids = inventory if args.tier == "full" else load_fast(fast(), inventory)
            counts = {c["id"]: c for c in cells}
            runs = sum(counts[i]["runs"] for i in ids)
            modeled = projection(ids, suite.counts[args.tier], suite, {i: c["runs"] for i, c in counts.items()}) if args.tier in suite.counts else None
            print(json.dumps({"tier": args.tier, "totals": totals(cells), "selected": {"cells": len(ids), "runs": runs,
                  "cancels": sum("-cancel-" in i for i in ids), "digest": digest(ids)}, "modeled_shard_seconds": modeled,
                  "cells": [{k: v for k, v in c.items() if k != "calls"} for c in cells]}, indent=1))
        elif args.command == "plan":
            document = plan(source, fast(), os.environ, suite)
            with open(args.output, "x", encoding="utf-8") as stream:
                json.dump(document, stream)
        elif args.command == "evidence":
            want = shard_expectation(args.tier, args.shard, args.sha, args.run_id, args.run_attempt, args.runner_os, source, fast(), suite)
            cells = read(args.cells) if os.path.exists(args.cells) else ""
            document = shard_evidence(want, cells, args.suite_exit)
            with open(args.output, "x", encoding="utf-8") as stream:
                json.dump(document, stream, indent=1)
                stream.write("\n")
            require(document["result"] == "pass", "the shard did not complete its cells exactly once")
    except (Refusal, OSError) as error:
        print("service qualification matrix: " + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

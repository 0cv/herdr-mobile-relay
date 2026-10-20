#!/usr/bin/env python3
"""Pure inventory, selection and evidence helper for service qualification.

It reads tests/test_native_install.sh as TEXT and enumerates the suite's cells by static
interpretation of its control flow (loops, conditions, f-string cell ids). Everything else is
skipped or evaluated as expressions over a whitelist of pure builtins: it never imports or runs a suite,
builds or starts anything, touches HOME, the network or a manager, and has no import-time effects.
A cell is one semantic id (the fixture name); the full tier is every cell, the fast tier the
explicit subset in tests/native_install_fast_cases.json. Shards, evidence and the aggregate gate
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
SHARD_COUNTS = {"fast": 2, "full": 8}
RUNNER_OS = {"macos-15": "Darwin", "ubuntu-24.04": "Linux"}
NATIVE = "tests/test_native_install.sh"
SUITES = ("tests/test_common.sh", "tests/test_service_credentials.sh", "tests/test_service_supervision.py",
          "tests/test_service_transport_dispatch.py", "tests/test_plugin_doctor.py", "tests/test_setup_menu.py",
          "tests/test_service_uninstall_cli.py", "tests/test_uninstall.sh",
          "tests/test_tailscale_cli_setup_failure.py", "tests/test_release_scripts.sh")
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
    (the pre-gate source); gates, when present, are evaluated as always taken and record nothing."""

    def __init__(self, program, legacy):
        self.tree = ast.parse(program)
        self.legacy = legacy
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
        return self.legacy and isinstance(n.func, ast.Attribute) and n.func.attr == "Fixture"

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
        elif self.legacy and isinstance(node.func, ast.Attribute) and node.func.attr == "Fixture":
            self.open(self.need(node.args[1], frame, "a fixture name"))

    def bind(self, target, value, frame):
        names = [target.id] if isinstance(target, ast.Name) else (
            [e.id for e in target.elts if isinstance(e, ast.Name)] if isinstance(target, (ast.Tuple, ast.List)) else [])
        try:
            items = [value] if isinstance(target, ast.Name) else list(value)
            require(len(items) == len(names) == len(getattr(target, "elts", names)), "shape")
        except Exception:
            items = [MISSING] * len(names)
        for name, item in zip(names, items):
            if item is MISSING:
                frame.pop(name, None)
            else:
                frame[name] = item

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


def enumerate_cells(text, legacy=False):
    """Ordered cells [{id, runs, probes}] of a suite source text; refuses anything it cannot resolve."""
    walker = Enumerator(suite_program(text), legacy)
    walker.block(walker.tree.body, {})
    ids = [cell["id"] for cell in walker.cells]
    require(len(ids) == len(set(ids)) == len({i.lower() for i in ids}), "cell ids must be unique, even on a case-insensitive volume")
    require(all(c["runs"] <= 1 for c in walker.cells), "a cell runs the installer at most once")
    return walker.cells


def digest(ids):
    return hashlib.sha256(("\n".join(sorted(ids)) + "\n").encode()).hexdigest()


def totals(cells):
    records = sorted("%s %d %d" % (c["id"], c["runs"], c["probes"]) for c in cells)  # every cell's own run and probe count
    return {"cells": len(cells), "runs": sum(c["runs"] for c in cells), "probes": sum(c["probes"] for c in cells),
            "cancels": sum("-cancel-" in c["id"] for c in cells), "digest": digest([c["id"] for c in cells]),
            "cell_digest": hashlib.sha256(("\n".join(records) + "\n").encode()).hexdigest()}


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


# Modeled Ubuntu seconds per cell after the shim change, and the launchd multiplier: the hosted per-class median
# of native-install-count_cells.txt divided by the measured CPU ratio (pre-shim hosted logs; probes, cancellation and
# parity are local-weight estimates). Used only to balance shards and to project; never a measured hosted time.
CLASS_SECONDS = {"capture-initial": (.29, 1.0), "capture-fresh": (1.15, 1.06), "capture-ready": (3.05, 1.25),
                 "capture-public": (2.12, 1.4), "install-ok": (1.5, 1.24), "early": (.41, 1.54), "quiesce": (3.54, 1.54),
                 "public": (3.48, 1.54), "ready": (4.75, 1.54), "refusal": (.1, 1.0), "origin-refusal": (.32, 1.0),
                 "probe": (.17, 1.16), "valid": (2.4, 1.3), "cancel": (4.75, 1.75), "parity": (8.0, 1.0)}
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
    seconds, launchd = CLASS_SECONDS[cost_class(a)]
    return round(seconds * (launchd if a.get("platform") == "launchd" else 1.0), 3)


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


def assign(ids, count):
    """Longest-processing-time balancing by modeled cost class; deterministic, shards disjoint and complete."""
    shards, loads = [[] for _ in range(count)], [0.0] * count
    for cell_id in sorted(ids, key=lambda i: (-cost(i), i)):
        target = loads.index(min(loads))
        shards[target].append(cell_id)
        loads[target] += cost(cell_id)
    order = {cell_id: n for n, cell_id in enumerate(ids)}
    return [sorted(shard, key=order.get) for shard in shards], loads


@functools.lru_cache(maxsize=8)
def _inventory(source_text):
    cells = enumerate_cells(source_text)
    seen = totals(cells)
    require(seen == FROZEN, "the suite inventory differs from the frozen baseline: %s != %s" % (seen, FROZEN))
    return tuple(c["id"] for c in cells)


def inventory_of(source_text):
    return list(_inventory(source_text))


def tier_ids(tier, source_text, fast_text):
    require(tier in SHARD_COUNTS, "unknown tier")
    inventory = inventory_of(source_text)
    return inventory, (inventory if tier == "full" else load_fast(fast_text, inventory))


def shard_ids(tier, shard, inventory, ids):
    """(index, count, ids) for SHARD 'i/n' (n must be the tier's fixed count); None selects the whole tier."""
    if not shard:
        return 0, 0, list(ids)
    m = re.fullmatch(r"([1-9][0-9]?)/([1-9][0-9]?)", shard)
    require(m and int(m[2]) == SHARD_COUNTS[tier] and 1 <= int(m[1]) <= int(m[2]), "invalid shard for this tier")
    return int(m[1]), int(m[2]), assign(ids, int(m[2]))[0][int(m[1]) - 1]


def plan(source_text, fast_text, env):
    tier = env.get("HERDR_NATIVE_INSTALL_TIER") or "full"
    inventory, ids = tier_ids(tier, source_text, fast_text)
    index, count, selected = shard_ids(tier, env.get("HERDR_NATIVE_INSTALL_SHARD") or "", inventory, ids)
    only = env.get("HERDR_NATIVE_INSTALL_CELL")
    if only:
        require(only in selected, "unknown or unselected cell id")
        selected = [only]
    return {"schema_version": SCHEMA, "tier": tier, "shard_index": index, "shard_count": count,
            "inventory": inventory, "selected": selected}


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


def shard_expectation(tier, shard, sha, run_id, attempt, runner_os, source_text, fast_text):
    inventory, ids = tier_ids(tier, source_text, fast_text)
    index, count, mine = shard_ids(tier, shard, inventory, ids)
    require(count, "a shard is required")
    return {"schema_version": SCHEMA, "kind": "native-install-shard", "candidate_sha": sha, "run_id": run_id,
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


def gate(tier, sha, run_id, attempt, directory, source_text, fast_text):
    """Every shard of every runner, plus the non-native suites, must be present, current and complete."""
    require(re.fullmatch(r"[0-9a-f]{40}", sha) and run_id.isdigit() and attempt.isdigit(), "malformed run identity")
    names, wanted = {}, {}
    for runner, runner_os in RUNNER_OS.items():
        names["service-suites-%s-%s-%s" % (tier, runner, sha)] = ("suites", runner_os, None)
        for shard in range(1, SHARD_COUNTS[tier] + 1):
            spec = "%d/%d" % (shard, SHARD_COUNTS[tier])
            names["service-native-%s-%s-s%d-%s" % (tier, runner, shard, sha)] = ("native", runner_os, spec)
    require(os.path.isdir(directory) and sorted(os.listdir(directory)) == sorted(names), "artifact set differs from the expected shards")
    for name, (mode, runner_os, spec) in sorted(names.items()):
        if mode == "suites":
            check_summary(read_artifact(directory, name, ["summary.json"])["summary.json"], "suites", SUITES, runner_os, sha, run_id, attempt)
            continue
        want = shard_expectation(tier, spec, sha, run_id, attempt, runner_os, source_text, fast_text)
        files = read_artifact(directory, name, ["native-shard-evidence.json", "summary.json"])
        doc = files["native-shard-evidence.json"]
        check_shard(doc, want)  # exactly this attempt: an artifact kept from an earlier attempt of the run is stale
        check_summary(files["summary.json"], "native", ["%s#%s-%dof%d" % (NATIVE, tier, want["shard_index"], want["shard_count"])],
                      runner_os, sha, run_id, attempt)
        wanted.setdefault(runner_os, []).extend(doc["completed"])
    ids = tier_ids(tier, source_text, fast_text)[1]
    for runner_os, completed in wanted.items():
        require(sorted(completed) == sorted(ids), "the %s shards do not cover the tier exactly once" % runner_os)
        kinds = {classify(c)["platform"] for c in completed if "platform" in classify(c)}
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
        p.add_argument("--source", default=NATIVE)
        p.add_argument("--fast-cases", default="tests/native_install_fast_cases.json")
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
        elif args.command == "list":
            text = read(args.source)
            cells = enumerate_cells(text, args.legacy)
            inventory = [c["id"] for c in cells]
            ids = inventory if args.tier == "full" else load_fast(read(args.fast_cases), inventory)
            counts = {c["id"]: c for c in cells}
            runs = sum(counts[i]["runs"] for i in ids)
            print(json.dumps({"tier": args.tier, "totals": totals(cells), "selected": {"cells": len(ids), "runs": runs,
                  "cancels": sum("-cancel-" in i for i in ids), "digest": digest(ids)}, "cells": cells}, indent=1))
        elif args.command == "plan":
            document = plan(read(args.source), read(args.fast_cases), os.environ)
            with open(args.output, "x", encoding="utf-8") as stream:
                json.dump(document, stream)
        elif args.command == "evidence":
            want = shard_expectation(args.tier, args.shard, args.sha, args.run_id, args.run_attempt, args.runner_os,
                                     read(args.source), read(args.fast_cases))
            cells = read(args.cells) if os.path.exists(args.cells) else ""
            document = shard_evidence(want, cells, args.suite_exit)
            with open(args.output, "x", encoding="utf-8") as stream:
                json.dump(document, stream, indent=1)
                stream.write("\n")
            require(document["result"] == "pass", "the shard did not complete its cells exactly once")
        else:
            gate(args.tier, args.sha, args.run_id, args.run_attempt, args.artifacts, read(args.source), read(args.fast_cases))
    except (Refusal, OSError) as error:
        print("service qualification matrix: " + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

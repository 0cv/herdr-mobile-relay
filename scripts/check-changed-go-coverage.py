#!/usr/bin/env python3
"""Prepare and validate exact AST-instrumented coverage for changed Go code."""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path


HUNK = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")
MARKER = re.compile(
    r"__HERDR_CHANGED_GO_COVERAGE__((?:function|statement|decision)\|[^|\s]+\|\d+\|\d+\|[^|\s]+\|(?:"
    r"(?:type-case|case|default)@\d+:\d+|no-match|default|hit|true|false|entered|empty"
    r"))"
)
CATEGORIES = ("functions", "statements", "decisions")


def changed_lines(diff: str) -> dict[str, set[int]]:
    result: dict[str, set[int]] = {}
    current: str | None = None
    new_line = 0
    for line in diff.splitlines():
        if line.startswith("+++ "):
            current = line[6:] if line.startswith("+++ b/") else None
            if current is not None:
                result.setdefault(current, set())
            continue
        match = HUNK.match(line)
        if match:
            new_line = int(match.group(1))
            continue
        if current is None or line.startswith("diff --git ") or line.startswith("--- "):
            continue
        if line.startswith("+") and not line.startswith("+++"):
            result[current].add(new_line)
            new_line += 1
        elif line.startswith("-") and not line.startswith("---"):
            continue
        elif line.startswith(" "):
            new_line += 1
    return {source: lines for source, lines in result.items() if lines}


def git_output(*args: str) -> str:
    return subprocess.run(["git", *args], check=True, text=True, stdout=subprocess.PIPE).stdout


def is_production_go(path: str) -> bool:
    return path.endswith(".go") and not path.endswith("_test.go") and not path.startswith("tests/")


def production_changes(base: str) -> dict[str, set[int]]:
    diff = git_output("diff", "--unified=0", "--no-color", base, "--", "*.go", ":(exclude)*_test.go")
    changed = {
        source: lines
        for source, lines in changed_lines(diff).items()
        if is_production_go(source)
    }
    for source in git_output("ls-files", "--others", "--exclude-standard", "--", "*.go").splitlines():
        if is_production_go(source):
            changed[source] = set(range(1, len(Path(source).read_text(encoding="utf-8").splitlines()) + 1))
    return dict(sorted(changed.items()))


def write_changes(path: Path, base: str) -> int:
    changed = production_changes(base)
    document = {
        "schema": 1,
        "base": base,
        "root": str(Path.cwd().resolve()),
        "files": [
            {"path": source, "lines": sorted(lines)}
            for source, lines in changed.items()
        ],
    }
    path.write_text(json.dumps(document, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return len(changed)


def parse_runtime_markers(output: str) -> set[str]:
    return {match.group(1) for match in MARKER.finditer(output)}


def record_location(record: dict) -> str:
    label = record.get("name") or record.get("kind") or record["id"]
    return f"{record['file']}:{record['line']}:{record['column']}:{label}"


def validate_observations(inventory: dict, observed: set[str]) -> list[str]:
    failures: list[str] = []
    expected: dict[str, tuple[str, dict, str]] = {}
    for category in CATEGORIES:
        singular = category[:-1]
        for record in inventory.get(category, []):
            markers = record.get("markers", [])
            if not markers:
                failures.append(f"changed {singular} has no runtime markers: {record_location(record)}")
            for marker in markers:
                outcome = marker.rpartition("|")[2]
                if marker in expected:
                    failures.append(f"duplicate AST inventory marker: {marker}")
                else:
                    expected[marker] = (singular, record, outcome)

    for decision in inventory.get("decisions", []):
        outcomes = {marker.rpartition("|")[2] for marker in decision.get("markers", [])}
        if decision.get("kind") in {"if", "for", "&&", "||", "case", "type-case", "case-condition"} and outcomes != {"true", "false"}:
            failures.append(f"boolean decision lacks true and false markers: {record_location(decision)}")
        if decision.get("kind") == "range" and outcomes != {"entered", "empty"}:
            failures.append(f"range decision lacks entered and empty markers: {record_location(decision)}")
        if decision.get("kind") in {"switch", "type-switch"} and len(outcomes) < 2:
            failures.append(f"switch decision lacks complete outcomes: {record_location(decision)}")

    for marker, (category, record, outcome) in sorted(expected.items()):
        if marker in observed:
            continue
        if category == "decision":
            failures.append(f"uncovered changed decision outcome: {record_location(record)}:{outcome}")
        else:
            failures.append(f"uncovered changed {category}: {record_location(record)}")
    for marker in sorted(observed - expected.keys()):
        failures.append(f"runtime emitted marker absent from AST inventory: {marker}")
    return failures


def self_test() -> None:
    diff = """diff --git a/a.go b/a.go
+++ b/a.go
@@ -1,3 +1,3 @@
 one
-old
+new
 three
"""
    assert changed_lines(diff) == {"a.go": {2}}
    inventory = {
        "functions": [],
        "statements": [],
        "decisions": [{
            "id": "decision|a.go|2|1|if",
            "file": "a.go",
            "line": 2,
            "column": 1,
            "kind": "if",
            "markers": ["decision|a.go|2|1|if|true", "decision|a.go|2|1|if|false"],
        }],
    }
    assert validate_observations(inventory, {"decision|a.go|2|1|if|true"}) == [
        "uncovered changed decision outcome: a.go:2:1:if:false"
    ]


def load_json(path: Path) -> dict:
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError(f"{path} must contain a JSON object")
    return value


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base")
    parser.add_argument("--config", type=Path, default=Path("tests/go-coverage-inventory.json"))
    parser.add_argument("--write-changes", type=Path)
    parser.add_argument("--inventory", type=Path)
    parser.add_argument("--test-output", type=Path)
    parser.add_argument("--print-packages", action="store_true")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()

    if args.self_test:
        self_test()
        print("changed Go coverage validator self-test passed")
        return 0
    if args.write_changes:
        if not args.base:
            parser.error("--write-changes requires --base")
        config = load_json(args.config)
        if config.get("schema") != 1 or config.get("base") != args.base:
            parser.error(f"coverage config base {config.get('base')!r} does not match {args.base!r}")
        count = write_changes(args.write_changes, args.base)
        print(f"prepared changed Go AST input for {count} file(s)")
        return 0
    if args.print_packages:
        if not args.inventory:
            parser.error("--print-packages requires --inventory")
        print("\n".join(load_json(args.inventory).get("packages", [])))
        return 0
    if not args.inventory or not args.test_output:
        parser.error("validation requires --inventory and --test-output")

    inventory = load_json(args.inventory)
    observed = parse_runtime_markers(args.test_output.read_text(encoding="utf-8"))
    failures = validate_observations(inventory, observed)
    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1
    functions = len(inventory.get("functions", []))
    statements = len(inventory.get("statements", []))
    decisions = len(inventory.get("decisions", []))
    outcomes = sum(len(record.get("markers", [])) for record in inventory.get("decisions", []))
    print(f"changed Go coverage passed: {functions}/{functions} functions, {statements}/{statements} statements, {outcomes}/{outcomes} outcomes across {decisions}/{decisions} decisions")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

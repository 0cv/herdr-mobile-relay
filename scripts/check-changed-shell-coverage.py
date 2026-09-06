#!/usr/bin/env python3
"""Require exact xtrace coverage and a finite branch/function inventory."""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


HUNK = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")
FUNCTION = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)\s*\(\)\s*\{")
TRACE = re.compile(r"^\++HERDR_XTRACE\|([^|]+)\|([^|]*)\|(\d+)\|(\d+)\|")
PRODUCTION_SHELL_PATHS = (
    "*.sh",
    ":(exclude)tests/**",
    ":(exclude)scripts/check-changed-*-coverage.sh",
)


def changed_lines(diff):
    result = {}
    current = None
    new_line = 0
    for line in diff.splitlines():
        if line.startswith("+++ b/"):
            current = line[6:]
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


def expand_line_specs(specs):
    result = set()
    for spec in specs:
        if "-" in spec:
            start, end = (int(value) for value in spec.split("-", 1))
            if start > end:
                raise ValueError(f"invalid descending line range: {spec}")
            result.update(range(start, end + 1))
        else:
            result.add(int(spec))
    return result


def relative_source(source, root):
    if not source:
        return None
    path = Path(source)
    if path.name == "install-functions.sh" and path.parent.name.startswith("herdr-install-test."):
        return "install.sh"
    if path.name in ("plugin-build-functions.sh", "plugin-build-tail.sh") and path.parent.name.startswith("herdr-plugin-build-coverage."):
        return "relay/plugin-build.sh"
    if path.name == "install-service-functions.sh" and path.parent.name.startswith("herdr-supervisor-service."):
        return "relay/install-service.sh"
    if path.name == "install-systemd-functions.sh" and path.parent.name.startswith("herdr-supervisor-service."):
        return "relay/install-systemd-user-service.sh"
    if path.name == "change-hostname-fragment.sh" and path.parent.name.startswith("herdr-hostname-coverage."):
        return "relay/change-hostname.sh"
    if (
        path.name == "package-release.sh"
        and path.parent.name == "scripts"
        and path.parent.parent.name == "package-repo"
        and path.parent.parent.parent.name.startswith("herdr-release-script-test.")
    ):
        return "scripts/package-release.sh"
    if (
        path.name in ("common.sh", "uninstall.sh")
        and path.parent.name == "relay"
        and path.parent.parent.name.startswith("herdr-uninstall-test.")
    ):
        return f"relay/{path.name}"
    if not path.is_absolute():
        path = root / path
    try:
        return path.resolve().relative_to(root.resolve()).as_posix()
    except ValueError:
        return None


def trace_observations(trace, root):
    hits = set()
    statuses = {}
    previous = {}
    for raw_line in trace.splitlines():
        match = TRACE.match(raw_line)
        if match is None:
            continue
        process = match.group(1)
        source = relative_source(match.group(2), root)
        line = int(match.group(3))
        prior_status = int(match.group(4))
        if process in previous:
            prior_source, prior_line = previous[process]
            if prior_source is not None:
                statuses.setdefault((prior_source, prior_line), set()).add(prior_status)
        previous[process] = (source, line)
        if source is not None:
            hits.add((source, line))
    return hits, statuses


def is_decision(line):
    stripped = line.strip()
    if stripped.startswith("' "):
        return False
    if re.match(r"^(if|elif|case|for|select|while|until)\b", stripped):
        return True
    if stripped.startswith("[") or stripped.startswith("! "):
        return True
    return " &&" in line or "||" in line


def multiline_single_quoted_lines(lines):
    ignored = set()
    single_quoted = False
    double_quoted = False
    for number, line in enumerate(lines, 1):
        if single_quoted:
            ignored.add(number)
        index = 0
        while index < len(line):
            character = line[index]
            if character == "\\" and not single_quoted:
                index += 2
                continue
            if character == "#" and not single_quoted and not double_quoted:
                break
            if character == "'" and not double_quoted:
                single_quoted = not single_quoted
            elif character == '"' and not single_quoted:
                double_quoted = not double_quoted
            index += 1
    return ignored


def function_ranges(lines):
    ranges = []
    active = None
    for number, line in enumerate(lines, 1):
        match = FUNCTION.match(line)
        if match and active is None:
            active = (match.group(1), number)
        if active and line == "}":
            ranges.append((active[0], active[1], number))
            active = None
    return ranges


def case_decisions(changed_numbers, lines):
    decisions = set()
    stack = []
    completed = []
    for number, line in enumerate(lines, 1):
        stripped = line.strip()
        if re.match(r"^case\b.*\bin\s*$", stripped):
            stack.append({"start": number, "arms": []})
            continue
        case_prefix = stripped.partition(")")[0]
        if stack and ")" in stripped and "$(" not in case_prefix and "((" not in case_prefix:
            stack[-1]["arms"].append(number)
        if re.match(r"^esac\b", stripped) and stack:
            completed.append(stack.pop())
    completed.extend(stack)
    for block in completed:
        if block["start"] in changed_numbers:
            decisions.add(block["start"])
        else:
            decisions.update(set(block["arms"]) & changed_numbers)
    return decisions


def source_inventory(changed, sources):
    functions = set()
    decisions = set()
    for source, changed_numbers in changed.items():
        lines = sources[source]
        ignored = multiline_single_quoted_lines(lines)
        decision_lines = ["" if number in ignored else line for number, line in enumerate(lines, 1)]
        ranges = function_ranges(lines)
        for name, start, end in ranges:
            if any(start <= number <= end for number in changed_numbers):
                functions.add((source, name))
        for number in changed_numbers:
            if number not in ignored and is_decision(lines[number - 1]):
                decisions.add((source, number))
        decisions.update((source, number) for number in case_decisions(changed_numbers, decision_lines))
    return functions, decisions


def parse_evidence(reference):
    source, separator, number = reference.rpartition(":")
    if not separator or not source or not number.isdigit():
        raise ValueError(f"invalid evidence reference: {reference}")
    return source, int(number)


def git_output(root, *args):
    return subprocess.run(
        ["git", *args], cwd=str(root), check=True, text=True, stdout=subprocess.PIPE
    ).stdout


def production_changes(root, base):
    diff = git_output(
        root,
        "diff",
        "--unified=0",
        "--no-color",
        base,
        "--",
        *PRODUCTION_SHELL_PATHS,
    )
    changed = changed_lines(diff)
    untracked = git_output(
        root,
        "ls-files",
        "--others",
        "--exclude-standard",
        "--",
        *PRODUCTION_SHELL_PATHS,
    ).splitlines()
    for source in untracked:
        line_count = len((root / source).read_text(encoding="utf-8").splitlines())
        changed[source] = set(range(1, line_count + 1))
    return changed


def run_trace_matrix(root, commands):
    environment = os.environ.copy()
    for name in (
        "COPILOT_GITHUB_TOKEN",
        "GH_TOKEN",
        "GITHUB_TOKEN",
        "HERDR_GITHUB_TOKEN_FILE",
    ):
        environment.pop(name, None)
    environment["BASH_ENV"] = str(root / "tests" / "xtrace-env.sh")
    environment["HERDR_SHELL_COVERAGE"] = "1"
    candidates = [os.environ.get("HERDR_SHELL_COVERAGE_BASH")]
    if sys.platform == "darwin":
        candidates.extend(["/opt/homebrew/bin/bash", "/usr/local/bin/bash"])
    candidates.append(shutil.which("bash"))
    coverage_bash = next(
        (
            candidate
            for candidate in candidates
            if candidate
            and Path(candidate).is_file()
            and subprocess.run(
                [candidate, "-c", "(( BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 1) ))"],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            ).returncode
            == 0
        ),
        None,
    )
    if coverage_bash is None:
        raise SystemExit("changed shell coverage requires Bash 4.1 or newer for BASH_XTRACEFD")
    environment["PATH"] = str(Path(coverage_bash).parent) + os.pathsep + environment["PATH"]
    environment["HERDR_SHELL_COVERAGE_BASH"] = coverage_bash
    all_hits = set()
    all_statuses = {}
    for command in commands:
        print(f"tracing changed shell coverage: {' '.join(command)}", flush=True)
        with tempfile.TemporaryFile(mode="w+b") as trace_file:
            environment["BASH_XTRACEFD"] = str(trace_file.fileno())
            command = [
                coverage_bash if value == "bash" and index == 0 else value
                for index, value in enumerate(command)
            ]
            result = subprocess.run(
                command,
                cwd=str(root),
                env=environment,
                pass_fds=(trace_file.fileno(),),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                start_new_session=True,
            )
            if result.returncode:
                sys.stderr.write(f"shell coverage command failed ({result.returncode}): {' '.join(command)}\n")
                sys.stderr.write(result.stdout)
                sys.stderr.write(result.stderr)
                raise SystemExit(1)
            trace_file.seek(0)
            trace = trace_file.read().decode("utf-8", errors="replace")
        hits, statuses = trace_observations(trace, root)
        all_hits.update(hits)
        for location, values in statuses.items():
            all_statuses.setdefault(location, set()).update(values)
    return all_hits, all_statuses


def validate(root, changed, hits, statuses, inventory):
    failures = []
    sources = {
        source: (root / source).read_text(encoding="utf-8").splitlines()
        for source in changed
    }
    non_executable = {}
    for source, specs in inventory.get("nonExecutable", {}).items():
        non_executable[source] = expand_line_specs(specs)
    declared_non_executable = {
        (source, number)
        for source, numbers in non_executable.items()
        for number in numbers
    }
    changed_pairs = {
        (source, number) for source, numbers in changed.items() for number in numbers
    }
    for pair in sorted(declared_non_executable - changed_pairs):
        failures.append(f"non-executable inventory row is not changed: {pair[0]}:{pair[1]}")
    for pair in sorted(declared_non_executable & hits):
        failures.append(f"line classified non-executable was traced: {pair[0]}:{pair[1]}")
    executable = changed_pairs - declared_non_executable
    for source, number in sorted(executable - hits):
        failures.append(f"uncovered changed executable shell line: {source}:{number}")

    expected_functions, expected_decisions = source_inventory(changed, sources)
    declared_functions = {
        (entry["file"], entry["name"]) for entry in inventory.get("functions", [])
    }
    for source, name in sorted(expected_functions - declared_functions):
        failures.append(f"changed shell function missing from inventory: {source}:{name}")
    for source, name in sorted(declared_functions - expected_functions):
        failures.append(f"function inventory row is not changed: {source}:{name}")
    for entry in inventory.get("functions", []):
        try:
            evidence = parse_evidence(entry["evidence"])
        except (KeyError, ValueError) as error:
            failures.append(str(error))
            continue
        ranges = [
            (start, end)
            for name, start, end in function_ranges(sources.get(entry.get("file"), []))
            if name == entry.get("name")
        ]
        if evidence[0] != entry.get("file") or not any(start <= evidence[1] <= end for start, end in ranges):
            failures.append(f"function evidence is outside changed function: {entry.get('file')}:{entry.get('name')} -> {entry.get('evidence')}")
        elif evidence not in hits:
            failures.append(f"function evidence was not traced: {entry['file']}:{entry['name']} -> {entry['evidence']}")

    declared_decisions = {
        (entry["file"], int(entry["line"])) for entry in inventory.get("decisions", [])
    }
    for source, number in sorted(expected_decisions - declared_decisions):
        failures.append(f"changed shell decision missing from inventory: {source}:{number}")
    for source, number in sorted(declared_decisions - expected_decisions):
        failures.append(f"decision inventory row is not changed: {source}:{number}")
    outcome_count = 0
    impossible_count = 0
    for entry in inventory.get("decisions", []):
        outcomes = entry.get("outcomes", [])
        impossible = entry.get("impossible", [])
        if len(outcomes) + len(impossible) < 2:
            failures.append(f"decision lacks paired outcomes: {entry['file']}:{entry['line']}")
        names = [outcome.get("name") for outcome in outcomes + impossible]
        if None in names or len(names) != len(set(names)):
            failures.append(f"decision outcomes are unnamed or duplicated: {entry['file']}:{entry['line']}")
        for outcome in outcomes:
            if "status" in outcome:
                observed = statuses.get((entry["file"], int(entry["line"])), set())
                expected = outcome["status"]
                matched = any(value != 0 for value in observed) if expected == "nonzero" else expected in observed
                if not matched:
                    failures.append(f"decision outcome status was not traced: {entry['file']}:{entry['line']}:{outcome.get('name')} -> {expected}")
            else:
                try:
                    evidence = parse_evidence(outcome["evidence"])
                except (KeyError, ValueError) as error:
                    failures.append(str(error))
                    continue
                if evidence not in hits:
                    failures.append(f"decision outcome evidence was not traced: {entry['file']}:{entry['line']}:{outcome.get('name')} -> {outcome['evidence']}")
        for outcome in impossible:
            if not outcome.get("reason"):
                failures.append(f"impossible decision outcome lacks a reason: {entry['file']}:{entry['line']}:{outcome.get('name')}")
        outcome_count += len(outcomes)
        impossible_count += len(impossible)

    return failures, len(executable), len(expected_functions), len(expected_decisions), outcome_count, impossible_count


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base", default="4eb61d54d455199d58a3f7257b9464920121cefb")
    parser.add_argument("--inventory", type=Path, default=Path("tests/shell-coverage-inventory.json"))
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    inventory_path = args.inventory if args.inventory.is_absolute() else root / args.inventory
    inventory = json.loads(inventory_path.read_text(encoding="utf-8"))
    if inventory.get("base") != args.base:
        parser.error(f"inventory base {inventory.get('base')!r} does not match {args.base!r}")
    commands = inventory.get("commands", [])
    if not commands:
        parser.error("inventory has no trace commands")
    changed = production_changes(root, args.base)
    hits, statuses = run_trace_matrix(root, commands)
    failures, lines, functions, decisions, outcomes, impossible = validate(root, changed, hits, statuses, inventory)
    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1
    print(
        f"changed shell coverage passed: {lines}/{lines} executable lines, "
        f"{functions}/{functions} functions, {outcomes} outcomes across "
        f"{decisions}/{decisions} decisions, {impossible} impossible outcome(s) inventoried"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

"""Required installed inert runner. Only fixed identities leave the Go event stream."""
import argparse
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import sys
import tempfile

# Legacy six-group API; selection always includes the supplement and promotion.
INVENTORY = None


def command(group, mode):
    if mode not in {"normal", "race"}:
        raise ValueError("unknown mode")
    roots = sorted(re.escape(name) for name in group["tests"] if "/" not in name)
    args = ["go", "test", "-json", "-count=1", "-timeout=5m"]
    if mode == "race":
        args.append("-race")
    if group["tagged"]:
        args.append("-tags=herdr_installed_inert_test")
    return args + [group["package"], "-run", "^(" + "|".join(roots) + ")$"]


def unique_object(pairs):
    if len(dict(pairs)) != len(pairs):
        raise ValueError("duplicate key")
    return dict(pairs)


def invalid_constant(value):
    raise ValueError("non-finite JSON")


def load_inventory(path):
    return json.loads(path.read_text(), object_pairs_hook=unique_object,
                      parse_constant=invalid_constant)


INVENTORY = load_inventory(Path(__file__).with_name("installed-inert-inventory.json"))
SUPPLEMENTAL = load_inventory(Path(__file__).with_name("installed-inert-supplemental-inventory.json"))
# Additive to the frozen six-plus-fifteen historical groups, never optional.
# Go testing rewrites the fixture's ASCII spaces to underscores.
PROMOTION = {
    "package": "github.com/0cv/herdr-mobile-relay/cmd/herdr-mobile-relay",
    "tagged": True,
    "tests": [
        "TestInstalledProductionPromotionInert",
        "TestInstalledProductionPromotionInert/owned_update_and_scoped_removal",
        "TestInstalledProductionPromotionInert/uncertain_stop_retains_authority",
        "TestInstalledProductionPromotionInert/consented_missing_route_removal",
    ],
}
PACKAGES = {"github.com/0cv/herdr-mobile-relay/" + suffix for suffix in (
    "internal/config", "internal/tailscalecli", "internal/app", "internal/deviceauth",
    "internal/localcontrol", "cmd/herdr-mobile-relay")}


def selection(os_name, base=None, supplemental=None):
    base = INVENTORY if base is None else base
    supplemental = SUPPLEMENTAL if supplemental is None else supplemental
    if os_name not in {"Darwin", "Linux"}:
        raise ValueError("unsupported OS")
    if (not isinstance(base, list) or len(base) != 6
            or not isinstance(supplemental, dict)
            or set(supplemental) != {"description", "schema_version", "countByOS", "groups"}
            or type(supplemental["schema_version"]) is not int
            or supplemental["schema_version"] != 1
            or not isinstance(supplemental["description"], str)
            or not isinstance(supplemental["countByOS"], dict)
            or supplemental["countByOS"] != {"Darwin": 550, "Linux": 552}
            or any(type(n) is not int for n in supplemental["countByOS"].values())
            or not isinstance(supplemental["groups"], list)
            or len(supplemental["groups"]) != 15):
        raise ValueError("invalid inventory schema")
    selected = {}
    for system in ("Darwin", "Linux"):
        groups, seen = [], set()
        for index, group in enumerate(base + supplemental["groups"] + [PROMOTION]):
            keys = {"package", "tagged", "tests"}
            if index == 15:  # frozen handoff group, never optional
                keys.add("os_tests")
            if (not isinstance(group, dict) or set(group) != keys
                    or group["package"] not in PACKAGES
                    or type(group["tagged"]) is not bool):
                raise ValueError("invalid group")
            lists = [group["tests"]]
            if "os_tests" in keys:
                if not isinstance(group["os_tests"], dict) or set(group["os_tests"]) != {"Darwin", "Linux"}:
                    raise ValueError("invalid OS coverage")
                lists.append(group["os_tests"][system])
            names = []
            for items in lists:
                if not isinstance(items, list) or not items:
                    raise ValueError("invalid tests")
                for name in items:
                    if (not isinstance(name, str) or len(name) > 256
                            or not re.fullmatch(r"Test[A-Za-z0-9_]+(?:/[A-Za-z0-9_.=-]+)*", name)):
                        raise ValueError("invalid test identity")
                    identity = (group["package"], group["tagged"], name)
                    if identity in seen:
                        raise ValueError("duplicate identity")
                    seen.add(identity)
                    names.append(name)
            if any(name.split("/")[0] not in names for name in names):
                raise ValueError("missing root")
            groups.append(dict(package=group["package"], tagged=group["tagged"], tests=names))
        if (sum(len(g["tests"]) for g in groups[:6]) != 183
                or sum(len(g["tests"]) for g in groups[6:21]) != supplemental["countByOS"][system]
                or len(groups) != 22 or len(groups[21]["tests"]) != 4
                or sum(len(g["tests"]) for g in groups) != {"Darwin": 737, "Linux": 739}[system]):
            raise ValueError("invalid coverage count")
        selected[system] = groups
    return selected[os_name]


def validate(stream, status, group):
    required = set(group["tests"])
    states = {}
    started = ended = False
    try:
        for line in stream:
            event = json.loads(line, object_pairs_hook=unique_object, parse_constant=invalid_constant)
            if not isinstance(event, dict) or event.get("Package") != group["package"] or ended:
                return False
            action, name = event.get("Action"), event.get("Test", "")
            if not isinstance(name, str):
                return False
            if action == "start" and not name:
                if started:
                    return False
                started = True
                continue
            if not started:
                return False
            if not name:
                if action == "output":
                    continue
                if action != "pass" or any(s != "pass" for s in states.values()):
                    return False
                ended = True
                continue
            if name not in required:
                return False
            if action == "run":
                if name in states:
                    return False
                states[name] = "run"
            elif action == "pass":
                if states.get(name) != "run":
                    return False
                states[name] = "pass"
            elif action in {"output", "pause", "cont"}:
                if name not in states:
                    return False
            else:
                return False
    except (ValueError, TypeError, UnicodeError):
        return False
    return status == 0 and started and ended and required == states.keys() and all(s == "pass" for s in states.values())


def execute(args):
    # Anonymous private temporary streams, never printed or uploaded.
    with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
        result = subprocess.run(args, stdout=output, stderr=errors, check=False)
        output.seek(0)
        return output.read().decode("utf-8"), result.returncode


def run_suite(mode, execute_command=execute):
    system = platform.system()
    if mode not in {"normal", "race"} or (system == "Darwin" and mode == "race"):
        raise ValueError("unsupported mode")
    groups = selection(system)
    results = []
    for group in groups:
        try:
            events, status = execute_command(command(group, mode))
            passed = validate(events.splitlines(), status, group)
        except (OSError, UnicodeError):
            passed = False
        results.append({"package": group["package"], "tagged": group["tagged"],
                        "required_tests": group["tests"], "result": "pass" if passed else "fail"})
    return results


def authorized():
    # Context consistency only; the workflow separately verifies checkout provenance.
    system = platform.system()
    return (system in {"Linux", "Darwin"}
            and os.environ.get("GITHUB_ACTIONS") == "true"
            and os.environ.get("RUNNER_ENVIRONMENT") == "github-hosted"
            and os.environ.get("RUNNER_OS") == {"Linux": "Linux", "Darwin": "macOS"}[system])


def main(argv=None, execute_command=execute):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=["normal", "race"], required=True)
    parser.add_argument("--os", choices=["Linux", "Darwin"], required=True)
    parser.add_argument("--sha", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--attempt", required=True)
    parser.add_argument("--evidence", required=True)
    args = parser.parse_args(argv)
    if (not authorized()
            or not re.fullmatch(r"[0-9a-f]{40}", args.sha)
            or not all(re.fullmatch(r"[1-9][0-9]*", x) for x in (args.run_id, args.attempt))
            or args.os != platform.system() or (args.os == "Darwin" and args.mode == "race")
            or args.sha != os.environ.get("GITHUB_SHA")
            or args.run_id != os.environ.get("GITHUB_RUN_ID")
            or args.attempt != os.environ.get("GITHUB_RUN_ATTEMPT")):
        return 1
    # Exclusive creation: stale evidence can never be reused or overwritten.
    try:
        with open(args.evidence, "x", encoding="utf-8") as receipt:
            os.chmod(args.evidence, 0o600)
            results = run_suite(args.mode, execute_command)
            passed = all(r["result"] == "pass" for r in results)
            json.dump(dict(schema_version=1, candidate_sha=args.sha, run_id=args.run_id,
                           run_attempt=args.attempt, os=args.os, mode=args.mode,
                           result="pass" if passed else "fail", invocations=results), receipt, indent=2)
            receipt.write("\n")
        return 0 if passed else 1
    except (OSError, ValueError, TypeError):
        return 1


if __name__ == "__main__":
    sys.exit(main())

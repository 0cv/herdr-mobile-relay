"""Explicit hosted-only real-Run lifecycle evidence; never part of inert defaults."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import sys

spec = importlib.util.spec_from_file_location("installed_inert_evidence", Path(__file__).with_name("installed_inert.py"))
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)

GROUP = {
    "package": "github.com/0cv/herdr-mobile-relay/internal/app",
    "tests": ["TestInstalledHostedRealRunBindFailure", "TestInstalledHostedRealRunShutdownRestart", "TestInstalledHostedRealRunCancelBeforeServe"],
}


GROUPS = [GROUP, {
    "package": "github.com/0cv/herdr-mobile-relay/internal/localcontrol",
    "tests": ["TestInstalledEndpointHostedRestart", "TestInstalledEndpointHostedDisconnect",
              "TestInstalledEndpointHostedReplacement"],
}]
TAGS = ["herdr_installed_inert_test", "herdr_installed_hosted_test"]


def authorized():
    return (os.environ.get("HERDR_INSTALLED_HOSTED_TEST") == "1"
            and os.environ.get("GITHUB_ACTIONS") == "true"
            and os.environ.get("RUNNER_ENVIRONMENT") == "github-hosted"
            and os.environ.get("RUNNER_OS") == {"Linux": "Linux", "Darwin": "macOS"}.get(platform.system())
            and platform.system() in {"Linux", "Darwin"})


def command(mode, group=GROUP):
    if mode not in {"normal", "race"}:
        raise ValueError("unknown hosted mode")
    args = ["go", "test", "-json", "-count=1", "-timeout=3m",
            "-tags=herdr_installed_inert_test,herdr_installed_hosted_test"]
    if mode == "race":
        args.append("-race")
    return args + [group["package"], "-run", "^(" + "|".join(re.escape(n) for n in group["tests"]) + ")$"]


def main(argv=None, execute_command=evidence.execute):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=["normal", "race"], required=True)
    parser.add_argument("--os", choices=["Linux", "Darwin"], required=True)
    parser.add_argument("--sha", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--attempt", required=True)
    parser.add_argument("--evidence", required=True)
    args = parser.parse_args(argv)
    if (not authorized() or args.os != platform.system()
            or (args.os == "Darwin" and args.mode == "race")
            or not re.fullmatch(r"[0-9a-f]{40}", args.sha)
            or not all(re.fullmatch(r"[1-9][0-9]*", x) for x in (args.run_id, args.attempt))
            or args.sha != os.environ.get("GITHUB_SHA")
            or args.run_id != os.environ.get("GITHUB_RUN_ID")
            or args.attempt != os.environ.get("GITHUB_RUN_ATTEMPT")):
        return 1
    try:
        with open(args.evidence, "x", encoding="utf-8") as receipt:
            os.chmod(args.evidence, 0o600)
            results = []
            for group in GROUPS:
                try:
                    events, status = execute_command(command(args.mode, group))
                    passed = evidence.validate(events.splitlines(), status, group)
                except (OSError, UnicodeError):
                    passed = False
                results.append(dict(package=group["package"], tags=TAGS,
                                    required_tests=group["tests"], result="pass" if passed else "fail"))
            passed = all(r["result"] == "pass" for r in results)
            json.dump(dict(schema_version=2, suite="installed-hosted-real-run-and-endpoint",
                           candidate_sha=args.sha, run_id=args.run_id,
                           run_attempt=args.attempt, os=args.os, mode=args.mode,
                           invocations=results, result="pass" if passed else "fail"), receipt, indent=2)
            receipt.write("\n")
        return 0 if passed else 1
    except OSError:
        return 1


if __name__ == "__main__":
    sys.exit(main())

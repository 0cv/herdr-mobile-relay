#!/usr/bin/env python3
"""Read-only import verification. No relay/test execution, network or file writes."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import sys


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args])


def verify(root, mode, plan):
    provenance = json.loads((plan / "handoff/provenance.json").read_text())
    records = json.loads((plan / "handoff/source-inventory.json").read_text())
    metadata = root / ".git"
    if metadata.is_symlink() or not metadata.is_dir():
        raise ValueError("import must be a standalone clone, not a linked worktree")
    if (metadata / "commondir").exists() or (metadata / "objects/info/alternates").exists():
        raise ValueError("shared Git metadata/object alternates are not allowed")
    actual_root = Path(os.fsdecode(git(root, "rev-parse", "--show-toplevel")).strip()).resolve()
    if actual_root != root:
        raise ValueError("--root must name the canonical checkout root")
    head = git(root, "rev-parse", "HEAD").decode().strip()
    if head != provenance["baseline_commit"]:
        raise ValueError("HEAD is not the pinned handoff commit")
    if git(root, "diff", "--cached", "--name-only", "-z"):
        raise ValueError("import verifier requires an unchanged index")
    workflow = ".github/workflows/tailscale-native-preflight.yml"
    expected_names = {row["path"] for row in records}
    if len(expected_names) != len(records):
        raise ValueError("inventory contains duplicate paths")
    names = {os.fsdecode(p) for p in git(root, "ls-files", "-co", "--exclude-standard", "-z").split(b"\0") if p}
    if names != expected_names | {workflow}:
        raise ValueError("source membership differs (missing/extra tracked or untracked file)")
    ignored = git(root, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
    if ignored:
        raise ValueError("ignored files are present: use a fresh import without runtime state or build caches")
    digest_key = "baseline_sha256" if mode == "baseline" else "sha256"
    for row in records:
        relative = Path(row["path"])
        if relative.is_absolute() or ".." in relative.parts:
            raise ValueError("invalid inventory path")
        path = root / relative
        for parent in path.parents:
            if parent == root:
                break
            if parent.is_symlink():
                raise ValueError("symlinked source parent: " + row["path"])
        info = path.lstat()
        if row["kind"] != "file" or not stat.S_ISREG(info.st_mode):
            raise ValueError("source file kind differs: " + row["path"])
        if format(stat.S_IMODE(info.st_mode), "04o") != row["mode"]:
            raise ValueError("source mode differs: " + row["path"])
        if hashlib.sha256(path.read_bytes()).hexdigest() != row[digest_key]:
            raise ValueError("source content differs: " + row["path"])
    wf = root / workflow
    workflow_info = wf.lstat()
    if not stat.S_ISREG(workflow_info.st_mode) or stat.S_IMODE(workflow_info.st_mode) != 0o644:
        raise ValueError("workflow kind/mode differs")
    if hashlib.sha256(wf.read_bytes()).hexdigest() != provenance["baseline_workflow_sha256"]:
        raise ValueError("baseline diagnostic workflow changed")
    changed = {os.fsdecode(p) for p in git(root, "diff", "--name-only", "-z").split(b"\0") if p}
    expected_changed = set() if mode == "baseline" else {r["path"] for r in provenance["local_only_delta"]}
    if changed != expected_changed:
        raise ValueError("unexpected unstaged diff set")
    print(f"Verified {len(records)} {mode} source paths, index, workflow and pinned commit.")
    print("Identity only: this is NOT source review, test success or native qualification.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--mode", choices=("baseline", "candidate"), default="baseline")
    args = parser.parse_args()
    try:
        verify(args.root.resolve(strict=True), args.mode, Path(__file__).resolve().parent)
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as exc:
        print("Import verification FAILED: " + str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

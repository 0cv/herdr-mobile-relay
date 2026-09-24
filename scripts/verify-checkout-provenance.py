#!/usr/bin/env python3
"""Verify a clean checkout against GitHub's event commit and committed tree.

This establishes reproducibility at the trusted event/checkout boundary; it
is not independent approval of repository-controlled workflow or source code.
"""
from __future__ import annotations

import argparse
import hashlib
import os
from pathlib import Path
import re
import stat
import subprocess
import sys


class ProvenanceError(Exception):
    pass


def git(root: Path, *args: str) -> bytes:
    result = subprocess.run(
        ["git", *args], cwd=root, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False
    )
    if result.returncode != 0:
        raise ProvenanceError("git metadata could not be verified")
    return result.stdout


def blob_hash(payload: bytes, object_format: str) -> str:
    digest = hashlib.new(object_format)
    digest.update(b"blob " + str(len(payload)).encode("ascii") + b"\0")
    digest.update(payload)
    return digest.hexdigest()


def file_blob_hash(path: bytes, size: int, object_format: str) -> str:
    digest = hashlib.new(object_format)
    digest.update(b"blob " + str(size).encode("ascii") + b"\0")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        fd = os.open(path, flags)
    except OSError as exc:
        raise ProvenanceError("tracked source file could not be read") from exc
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_size != size:
            raise ProvenanceError("tracked source file changed during verification")
        while True:
            chunk = os.read(fd, 1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
    finally:
        os.close(fd)
    return digest.hexdigest()


def tracked_tree(root: Path) -> dict[bytes, tuple[bytes, bytes]]:
    records = git(root, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
    entries: dict[bytes, tuple[bytes, bytes]] = {}
    for record in records.split(b"\0"):
        if not record:
            continue
        try:
            header, path = record.split(b"\t", 1)
            mode, object_type, oid = header.split(b" ")
        except ValueError as exc:
            raise ProvenanceError("Git source tree is malformed") from exc
        if object_type != b"blob" or mode not in (b"100644", b"100755", b"120000"):
            raise ProvenanceError("Git source tree contains an unsupported entry")
        if path in entries:
            raise ProvenanceError("Git source tree contains a duplicate path")
        entries[path] = (mode, oid)
    return entries


def worktree_entries(root: Path) -> dict[bytes, tuple[bytes, os.stat_result]]:
    root_bytes = os.fsencode(root)
    entries: dict[bytes, tuple[bytes, os.stat_result]] = {}
    pending = [b""]
    while pending:
        parent = pending.pop()
        directory = root_bytes if not parent else os.path.join(root_bytes, parent)
        try:
            with os.scandir(directory) as children:
                for child in children:
                    name = child.name
                    if not parent and name == b".git":
                        continue
                    relative = name if not parent else parent + b"/" + name
                    try:
                        info = child.stat(follow_symlinks=False)
                    except OSError as exc:
                        raise ProvenanceError("checkout membership could not be read") from exc
                    if stat.S_ISDIR(info.st_mode):
                        pending.append(relative)
                    elif stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
                        entries[relative] = (b"120000" if stat.S_ISLNK(info.st_mode) else b"file", info)
                    else:
                        raise ProvenanceError("checkout contains an unsupported file kind")
        except OSError as exc:
            raise ProvenanceError("checkout membership could not be read") from exc
    return entries


def verify(root: Path, expected_sha: str) -> dict[str, str | int]:
    root = root.resolve(strict=True)
    if not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", expected_sha):
        raise ProvenanceError("expected source commit is not a full Git object ID")
    try:
        top = Path(os.fsdecode(git(root, "rev-parse", "--show-toplevel").rstrip(b"\n"))).resolve(strict=True)
    except OSError as exc:
        raise ProvenanceError("checkout root could not be resolved") from exc
    if top != root:
        raise ProvenanceError("verification root is not the repository root")
    commit = git(root, "rev-parse", "HEAD").decode("ascii").strip()
    if commit != expected_sha:
        raise ProvenanceError("checkout HEAD does not match the expected source commit")
    if git(root, "status", "--porcelain=v1", "--untracked-files=all"):
        raise ProvenanceError("checkout index or worktree is not clean")
    if git(root, "ls-files", "--others", "--ignored", "--exclude-standard", "-z"):
        raise ProvenanceError("checkout contains undeclared ignored files")

    object_format = git(root, "rev-parse", "--show-object-format").decode("ascii").strip()
    if object_format not in ("sha1", "sha256"):
        raise ProvenanceError("repository uses an unsupported Git object format")
    expected = tracked_tree(root)
    actual = worktree_entries(root)
    if actual.keys() != expected.keys():
        raise ProvenanceError("checkout source membership differs from the committed Git tree")

    root_bytes = os.fsencode(root)
    source_digest = hashlib.sha256()
    for path in sorted(expected):
        mode, oid = expected[path]
        actual_kind, info = actual[path]
        full_path = os.path.join(root_bytes, path)
        if mode == b"120000":
            if actual_kind != b"120000":
                raise ProvenanceError("tracked source file kind differs from the Git tree")
            try:
                target = os.readlink(full_path)
                resolved = os.path.realpath(full_path)
                if not os.path.exists(resolved) or os.path.commonpath((root_bytes, resolved)) != root_bytes:
                    raise ProvenanceError("source symlink escapes or is missing from the checkout")
            except (OSError, ValueError) as exc:
                raise ProvenanceError("source symlink could not be verified") from exc
            target_relative = os.path.relpath(resolved, root_bytes)
            if target_relative == b".git" or target_relative.startswith(b".git/"):
                raise ProvenanceError("source symlink targets Git metadata")
            if target_relative != b"." and target_relative not in expected:
                prefix = target_relative.rstrip(b"/") + b"/"
                if not any(candidate.startswith(prefix) for candidate in expected):
                    raise ProvenanceError("source symlink targets undeclared checkout content")
            actual_oid = blob_hash(target, object_format)
        else:
            if actual_kind != b"file":
                raise ProvenanceError("tracked source file kind differs from the Git tree")
            expected_mode = 0o755 if mode == b"100755" else 0o644
            if stat.S_IMODE(info.st_mode) != expected_mode:
                raise ProvenanceError("tracked source file mode differs from the Git tree")
            actual_oid = file_blob_hash(full_path, info.st_size, object_format)
        if actual_oid.encode("ascii") != oid:
            raise ProvenanceError("tracked source bytes differ from the committed Git tree")
        source_digest.update(mode + b"\0" + oid + b"\0" + len(path).to_bytes(8, "big") + path)

    tree = git(root, "rev-parse", "HEAD^{tree}").decode("ascii").strip()
    return {
        "commit": commit,
        "tree": tree,
        "files": len(expected),
        "source_sha256": source_digest.hexdigest(),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=os.environ.get("GITHUB_WORKSPACE", "."))
    parser.add_argument("--expected-sha", default=os.environ.get("GITHUB_SHA", ""))
    args = parser.parse_args()
    try:
        evidence = verify(Path(args.root), args.expected_sha)
    except (OSError, ProvenanceError) as exc:
        print(f"checkout provenance refused: {exc}", file=sys.stderr)
        return 1

    line = (
        f"checkout provenance verified: commit={evidence['commit']} "
        f"tree={evidence['tree']} files={evidence['files']} "
        f"source_sha256={evidence['source_sha256']}"
    )
    print(line)
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as handle:
            handle.write("## Checkout provenance\n\n" + line + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

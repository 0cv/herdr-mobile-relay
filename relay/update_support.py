#!/usr/bin/env python3
"""Version checks and guarded self-updates for Herdr Mobile Relay."""

from __future__ import annotations

import fcntl
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path


PLUGIN_ID = "herdr-mobile-relay.events"
UPSTREAM_SOURCE = "0cv/herdr-mobile-relay"
UPSTREAM_GIT_URL = "https://github.com/0cv/herdr-mobile-relay.git"
UPSTREAM_MANIFEST_URL = (
    "https://raw.githubusercontent.com/0cv/herdr-mobile-relay/{revision}/herdr-plugin.toml"
)
UPDATE_STATE_NAME = "update-state.json"
UPDATE_LOCK_NAME = "update.lock"
SEMVER_RE = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")
MANIFEST_VERSION_RE = re.compile(r'^version = "([^"]+)"$', re.MULTILINE)
CANONICAL_REMOTE_RE = re.compile(
    r"^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)"
    r"0cv/herdr-mobile-relay(?:\.git)?/?$",
    re.IGNORECASE,
)
UPDATE_STATES = {
    "checking",
    "current",
    "available",
    "blocked",
    "scheduled",
    "installing",
    "restarting",
    "succeeded",
    "failed",
    "rolled_back",
}


def compact_error(value: object, limit: int = 500) -> str:
    return " ".join(str(value or "").split())[:limit]


def semver(value: object) -> tuple[int, int, int] | None:
    match = SEMVER_RE.fullmatch(str(value or "").strip())
    if not match:
        return None
    return tuple(int(part) for part in match.groups())


def manifest_version(content: str) -> str:
    match = MANIFEST_VERSION_RE.search(content)
    value = match.group(1) if match else ""
    if not semver(value):
        raise ValueError("manifest version must use MAJOR.MINOR.PATCH")
    return value


def product_version(repo_root: Path) -> str:
    try:
        return manifest_version((repo_root / "herdr-plugin.toml").read_text(encoding="utf-8"))
    except OSError:
        return "unknown"


def run_command(
    argv: list[str],
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
    timeout: float = 120,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        argv,
        cwd=str(cwd) if cwd else None,
        env=env,
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )


def git_output(repo_root: Path, *args: str, timeout: float = 10) -> str:
    result = run_command(["git", "-C", str(repo_root), *args], timeout=timeout)
    if result.returncode != 0:
        raise RuntimeError(compact_error(result.stderr or result.stdout or "git command failed"))
    return result.stdout.strip()


def git_revision(repo_root: Path, *, dirty_suffix: bool = False) -> str:
    try:
        revision = git_output(repo_root, "rev-parse", "--short", "HEAD")
        if dirty_suffix and git_output(
            repo_root, "status", "--porcelain", "--untracked-files=normal"
        ):
            return f"{revision}-dirty"
        return revision
    except (OSError, RuntimeError, subprocess.SubprocessError):
        return "unknown"


def state_file(runtime_dir: Path) -> Path:
    return runtime_dir / UPDATE_STATE_NAME


def write_json_atomic(path: Path, payload: dict[str, object]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    os.chmod(path.parent, 0o700)
    descriptor, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, indent=2, sort_keys=True)
            handle.write("\n")
        os.chmod(temp_path, 0o600)
        os.replace(temp_path, path)
    finally:
        try:
            temp_path.unlink()
        except FileNotFoundError:
            pass


def read_update_state(
    runtime_dir: Path,
    current_version: str,
    current_revision: str,
) -> dict[str, object]:
    fallback: dict[str, object] = {
        "state": "checking",
        "current_version": current_version,
        "current_revision": current_revision,
        "available_version": "",
        "available_revision": "",
        "target_revision": "",
        "checked_at": 0,
        "can_install": False,
        "mode": "",
        "reason": "",
        "error": "",
    }
    try:
        loaded = json.loads(state_file(runtime_dir).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return fallback
    if not isinstance(loaded, dict) or loaded.get("state") not in UPDATE_STATES:
        return fallback
    result = fallback | {
        key: loaded.get(key, fallback[key])
        for key in fallback
    }
    result["current_version"] = current_version
    result["current_revision"] = current_revision
    for key in ("available_version", "available_revision", "target_revision", "mode", "reason", "error"):
        result[key] = compact_error(result.get(key), 500)
    result["checked_at"] = int(result.get("checked_at") or 0)
    result["can_install"] = result.get("can_install") is True
    return result


def _service_file(home: Path, system: str) -> Path | None:
    if system == "Linux":
        return home / ".config/systemd/user/herdr-mobile-relay.service"
    if system == "Darwin":
        return home / "Library/LaunchAgents/com.herdr-mobile-relay.service.plist"
    return None


def service_uses_repo(repo_root: Path, *, home: Path | None = None, system: str | None = None) -> bool:
    home = home or Path.home()
    system = system or platform.system()
    service_path = _service_file(home, system)
    if not service_path:
        return False
    try:
        content = service_path.read_text(encoding="utf-8")
    except OSError:
        return False
    root = str(repo_root.resolve())
    return root in content and str(repo_root.resolve() / "relay/herdr-mobile-relay-service.sh") in content


def managed_plugin_root(repo_root: Path | None, *, home: Path | None = None) -> Path | None:
    registry = (home or Path.home()) / ".config/herdr/plugins.json"
    try:
        entries = json.loads(registry.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if not isinstance(entries, list):
        return None
    resolved_root = repo_root.resolve() if repo_root else None
    for entry in entries:
        if not isinstance(entry, dict) or entry.get("plugin_id") != PLUGIN_ID:
            continue
        source = entry.get("source") if isinstance(entry.get("source"), dict) else {}
        try:
            plugin_root = Path(str(entry.get("plugin_root", ""))).resolve()
        except OSError:
            continue
        if (
            (resolved_root is None or plugin_root == resolved_root)
            and source.get("kind") == "github"
            and source.get("owner") == "0cv"
            and source.get("repo") == "herdr-mobile-relay"
        ):
            return plugin_root
    return None


def managed_plugin(repo_root: Path, *, home: Path | None = None) -> bool:
    return managed_plugin_root(repo_root, home=home) is not None


def inspect_installation(
    repo_root: Path,
    herdr_bin: str,
    *,
    home: Path | None = None,
    system: str | None = None,
) -> dict[str, str | bool]:
    home = home or Path.home()
    system = system or platform.system()
    if system not in {"Linux", "Darwin"}:
        return {"can_install": False, "mode": "", "reason": f"{system} updates are unsupported"}
    if not service_uses_repo(repo_root, home=home, system=system):
        return {
            "can_install": False,
            "mode": "",
            "reason": "Self-update requires the stable background service for this checkout",
        }
    if managed_plugin(repo_root, home=home):
        if not Path(herdr_bin).is_file() and not shutil.which(herdr_bin):
            return {"can_install": False, "mode": "managed", "reason": "Herdr is unavailable"}
        return {"can_install": True, "mode": "managed", "reason": ""}
    try:
        remote = git_output(repo_root, "config", "--get", "remote.origin.url")
        branch = git_output(repo_root, "branch", "--show-current")
        status = git_output(repo_root, "status", "--porcelain", "--untracked-files=normal")
    except (OSError, RuntimeError, subprocess.SubprocessError) as exc:
        return {"can_install": False, "mode": "local", "reason": compact_error(exc)}
    if not CANONICAL_REMOTE_RE.fullmatch(remote):
        return {
            "can_install": False,
            "mode": "local",
            "reason": "The checkout origin is not the canonical 0cv/herdr-mobile-relay repository",
        }
    if branch != "main":
        return {"can_install": False, "mode": "local", "reason": "The checkout must be on main"}
    if status:
        return {
            "can_install": False,
            "mode": "local",
            "reason": "The checkout has local changes; commit or remove them before updating",
        }
    return {"can_install": True, "mode": "local", "reason": ""}


def fetch_remote_release(timeout: float = 10) -> tuple[str, str]:
    result = run_command(
        ["git", "ls-remote", "--exit-code", UPSTREAM_GIT_URL, "refs/heads/main"],
        timeout=timeout,
    )
    if result.returncode != 0:
        raise RuntimeError(compact_error(result.stderr or result.stdout or "could not read upstream main"))
    fields = result.stdout.split()
    revision = fields[0].lower() if fields else ""
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise RuntimeError("upstream returned an invalid Git revision")
    request = urllib.request.Request(
        UPSTREAM_MANIFEST_URL.format(revision=revision),
        headers={"User-Agent": "herdr-mobile-relay-update-check"},
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            content = response.read(65537)
    except (OSError, urllib.error.URLError) as exc:
        raise RuntimeError(f"could not read upstream manifest: {compact_error(exc)}") from exc
    if len(content) > 65536:
        raise RuntimeError("upstream manifest is unexpectedly large")
    try:
        version = manifest_version(content.decode("utf-8"))
    except (UnicodeDecodeError, ValueError) as exc:
        raise RuntimeError(compact_error(exc)) from exc
    return revision, version


def check_for_update(
    repo_root: Path,
    runtime_dir: Path,
    herdr_bin: str,
    *,
    now: int | None = None,
    remote_release: tuple[str, str] | None = None,
    home: Path | None = None,
    system: str | None = None,
) -> dict[str, object]:
    current_version = product_version(repo_root)
    current_revision = git_revision(repo_root)
    previous = read_update_state(runtime_dir, current_version, current_revision)
    checking = previous | {"state": "checking", "error": ""}
    write_json_atomic(state_file(runtime_dir), checking)
    try:
        available_revision, available_version = remote_release or fetch_remote_release()
        current_semver = semver(current_version)
        available_semver = semver(available_version)
        if current_semver is None or available_semver is None:
            raise RuntimeError("installed and available versions must use MAJOR.MINOR.PATCH")
        eligibility = inspect_installation(
            repo_root,
            herdr_bin,
            home=home,
            system=system,
        )
        newer = available_semver > current_semver
        state = "available" if newer and eligibility["can_install"] else "blocked" if newer else "current"
        status: dict[str, object] = {
            "state": state,
            "current_version": current_version,
            "current_revision": current_revision,
            "available_version": available_version if newer else "",
            "available_revision": available_revision[:12] if newer else "",
            "target_revision": available_revision if newer else "",
            "checked_at": int(now if now is not None else time.time()),
            "can_install": bool(newer and eligibility["can_install"]),
            "mode": str(eligibility["mode"]),
            "reason": str(eligibility["reason"]) if newer else "",
            "error": "",
        }
    except Exception as exc:
        status = previous | {
            "state": "failed",
            "current_version": current_version,
            "current_revision": current_revision,
            "error": compact_error(exc),
        }
    write_json_atomic(state_file(runtime_dir), status)
    return status


def _runner_environment(job: dict[str, object]) -> dict[str, str]:
    env = os.environ.copy()
    env["HERDR_MOBILE_RELAY_NO_AUTO_SETUP"] = "1"
    relay_env = str(job.get("relay_env") or "")
    if relay_env:
        env["HERDR_RELAY_ENV"] = relay_env
    return env


def _install_managed(job: dict[str, object], revision: str) -> None:
    result = run_command(
        [
            str(job["herdr_bin"]),
            "plugin",
            "install",
            UPSTREAM_SOURCE,
            "--ref",
            revision,
            "--yes",
        ],
        env=_runner_environment(job),
        timeout=300,
    )
    if result.returncode != 0:
        raise RuntimeError(compact_error(result.stderr or result.stdout or "Herdr plugin install failed"))
    registered_root = managed_plugin_root(
        None,
        home=Path(str(job["home"])),
    )
    if registered_root is None:
        raise RuntimeError("Herdr did not register the updated managed plugin")
    job["repo_root"] = str(registered_root)


def _install_local(job: dict[str, object], revision: str) -> None:
    repo_root = Path(str(job["repo_root"]))
    eligibility = inspect_installation(
        repo_root,
        str(job["herdr_bin"]),
        home=Path(str(job["home"])),
        system=str(job["system"]),
    )
    if not eligibility["can_install"] or eligibility["mode"] != "local":
        raise RuntimeError(str(eligibility["reason"]) or "local checkout is no longer eligible")
    fetched = run_command(
        ["git", "-C", str(repo_root), "fetch", "--no-tags", UPSTREAM_GIT_URL, "refs/heads/main"],
        timeout=180,
    )
    if fetched.returncode != 0:
        raise RuntimeError(compact_error(fetched.stderr or fetched.stdout or "git fetch failed"))
    fetched_revision = git_output(repo_root, "rev-parse", "FETCH_HEAD")
    if fetched_revision != revision:
        raise RuntimeError("upstream main changed; check for updates again")
    ancestor = run_command(
        ["git", "-C", str(repo_root), "merge-base", "--is-ancestor", "HEAD", revision],
        timeout=15,
    )
    if ancestor.returncode != 0:
        raise RuntimeError("the local checkout cannot fast-forward to the advertised update")
    merged = run_command(
        ["git", "-C", str(repo_root), "merge", "--ff-only", revision],
        timeout=120,
    )
    if merged.returncode != 0:
        raise RuntimeError(compact_error(merged.stderr or merged.stdout or "git fast-forward failed"))


def _verify_target(repo_root: Path, version: str, revision: str) -> None:
    if product_version(repo_root) != version:
        raise RuntimeError("installed manifest version does not match the advertised update")
    installed_revision = git_output(repo_root, "rev-parse", "HEAD")
    if installed_revision != revision:
        raise RuntimeError("installed Git revision does not match the advertised update")


def _install_service(job: dict[str, object]) -> None:
    script = Path(str(job["repo_root"])) / "relay/service.sh"
    result = run_command(
        ["bash", str(script), "install"],
        cwd=Path(str(job["repo_root"])),
        env=_runner_environment(job),
        timeout=120,
    )
    if result.returncode != 0:
        raise RuntimeError(compact_error(result.stderr or result.stdout or "relay service failed health verification"))


def _relay_port(relay_env: str) -> int:
    try:
        content = Path(relay_env).read_text(encoding="utf-8")
    except OSError:
        return 8375
    match = re.search(
        r"^HERDR_RELAY_PORT=(?:'|\")?([0-9]+)(?:'|\")?\s*$",
        content,
        re.MULTILINE,
    )
    return int(match.group(1)) if match else 8375


def _verify_running_health(job: dict[str, object], version: str, revision: str) -> None:
    port = _relay_port(str(job.get("relay_env") or ""))
    request = urllib.request.Request(
        f"http://127.0.0.1:{port}/healthz",
        headers={"User-Agent": "herdr-mobile-relay-updater"},
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            health = json.loads(response.read(65537))
    except (OSError, ValueError, urllib.error.URLError) as exc:
        raise RuntimeError(f"updated relay health check failed: {compact_error(exc)}") from exc
    if not isinstance(health, dict) or health.get("status") != "ok" or not health.get("instance"):
        raise RuntimeError("updated relay returned incomplete health metadata")
    if health.get("release_version") != version:
        raise RuntimeError("running relay release version does not match the installed update")
    running_revision = str(health.get("revision") or health.get("version") or "")
    if (
        not running_revision
        or running_revision.endswith("-dirty")
        or not revision.startswith(running_revision)
    ):
        raise RuntimeError("running relay revision does not match the installed update")


def _rollback(job: dict[str, object]) -> None:
    repo_root = Path(str(job["repo_root"]))
    previous_revision = str(job["previous_revision"])
    if job["mode"] == "managed":
        _install_managed(job, previous_revision)
    else:
        if git_output(repo_root, "rev-parse", "HEAD") != str(job["target_revision"]):
            raise RuntimeError("checkout changed during rollback")
        if git_output(repo_root, "status", "--porcelain", "--untracked-files=normal"):
            raise RuntimeError("checkout changed during rollback")
        reset = run_command(
            ["git", "-C", str(repo_root), "reset", "--hard", previous_revision],
            timeout=120,
        )
        if reset.returncode != 0:
            raise RuntimeError(compact_error(reset.stderr or reset.stdout or "git rollback failed"))
    _verify_target(repo_root, str(job["previous_version"]), previous_revision)
    _install_service(job)
    _verify_running_health(job, str(job["previous_version"]), previous_revision)


def run_update_job(job_path: Path) -> int:
    try:
        job = json.loads(job_path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        print(f"Invalid update job: {exc}", file=sys.stderr)
        return 2
    runtime_dir = Path(str(job["runtime_dir"]))
    lock_path = runtime_dir / UPDATE_LOCK_NAME
    lock_handle = lock_path.open("a+", encoding="utf-8")
    try:
        fcntl.flock(lock_handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        lock_handle.close()
        try:
            job_path.unlink()
        except OSError:
            pass
        return 3
    lock_handle.seek(0)
    lock_handle.truncate()
    lock_handle.write(f"{os.getpid()}\n")
    lock_handle.flush()
    os.chmod(lock_path, 0o600)
    try:
        base = {
            "current_version": str(job["previous_version"]),
            "current_revision": str(job["previous_revision"])[:12],
            "available_version": str(job["target_version"]),
            "available_revision": str(job["target_revision"])[:12],
            "target_revision": str(job["target_revision"]),
            "checked_at": int(job["checked_at"]),
            "can_install": False,
            "mode": str(job["mode"]),
            "reason": "",
            "error": "",
        }
        write_json_atomic(state_file(runtime_dir), base | {"state": "installing"})
        try:
            if job["mode"] == "managed":
                _install_managed(job, str(job["target_revision"]))
            else:
                _install_local(job, str(job["target_revision"]))
            repo_root = Path(str(job["repo_root"]))
            _verify_target(repo_root, str(job["target_version"]), str(job["target_revision"]))
            write_json_atomic(state_file(runtime_dir), base | {"state": "restarting"})
            _install_service(job)
            _verify_running_health(
                job,
                str(job["target_version"]),
                str(job["target_revision"]),
            )
            write_json_atomic(
                state_file(runtime_dir),
                base | {
                    "state": "succeeded",
                    "current_version": str(job["target_version"]),
                    "current_revision": str(job["target_revision"])[:12],
                    "available_version": "",
                    "available_revision": "",
                    "target_revision": "",
                },
            )
            return 0
        except Exception as update_error:
            try:
                _rollback(job)
            except Exception as rollback_error:
                write_json_atomic(
                    state_file(runtime_dir),
                    base | {
                        "state": "failed",
                        "error": (
                            f"Update failed: {compact_error(update_error)}. "
                            f"Rollback failed: {compact_error(rollback_error)}"
                        )[:500],
                    },
                )
                return 1
            write_json_atomic(
                state_file(runtime_dir),
                base | {
                    "state": "rolled_back",
                    "available_version": "",
                    "available_revision": "",
                    "target_revision": "",
                    "error": f"Update failed and was rolled back: {compact_error(update_error)}",
                },
            )
            return 1
    finally:
        fcntl.flock(lock_handle, fcntl.LOCK_UN)
        lock_handle.close()
        try:
            job_path.unlink()
        except OSError:
            pass


def launch_update_job(
    repo_root: Path,
    runtime_dir: Path,
    herdr_bin: str,
    relay_env: str,
    status: dict[str, object],
    *,
    python: str | None = None,
    home: Path | None = None,
    system: str | None = None,
) -> str:
    target_revision = str(status.get("target_revision") or "")
    if not re.fullmatch(r"[0-9a-f]{40}", target_revision):
        raise RuntimeError("update target is missing or invalid")
    home = home or Path.home()
    system = system or platform.system()
    runtime_dir.mkdir(parents=True, exist_ok=True)
    os.chmod(runtime_dir, 0o700)
    job_path = runtime_dir / f"update-job-{int(time.time() * 1000)}.json"
    job = {
        "repo_root": str(repo_root.resolve()),
        "runtime_dir": str(runtime_dir.resolve()),
        "herdr_bin": str(Path(herdr_bin).resolve()) if Path(herdr_bin).exists() else herdr_bin,
        "relay_env": relay_env,
        "home": str(home.resolve()),
        "system": system,
        "mode": str(status["mode"]),
        "previous_version": str(status["current_version"]),
        "previous_revision": git_output(repo_root, "rev-parse", "HEAD"),
        "target_version": str(status["available_version"]),
        "target_revision": target_revision,
        "checked_at": int(status["checked_at"]),
    }
    write_json_atomic(job_path, job)
    runner = str(Path(__file__).resolve())
    python = python or sys.executable
    label = f"herdr-mobile-relay-update-{int(time.time())}"
    if system == "Linux":
        systemd_run = shutil.which("systemd-run")
        if not systemd_run:
            raise RuntimeError("systemd-run is required for safe background updates")
        command = [
            systemd_run,
            "--user",
            "--collect",
            f"--unit={label}",
            "--property=Type=exec",
            python,
            runner,
            "--run-job",
            str(job_path),
        ]
    elif system == "Darwin":
        launchctl = shutil.which("launchctl") or "/bin/launchctl"
        command = [
            launchctl,
            "submit",
            "-l",
            label.replace("-", "."),
            "--",
            python,
            runner,
            "--run-job",
            str(job_path),
        ]
    else:
        raise RuntimeError(f"{system} updates are unsupported")
    result = run_command(command, timeout=20)
    if result.returncode != 0:
        try:
            job_path.unlink()
        except OSError:
            pass
        raise RuntimeError(compact_error(result.stderr or result.stdout or "could not start update job"))
    return label


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--run-job":
        raise SystemExit(run_update_job(Path(sys.argv[2])))
    raise SystemExit(f"Usage: {Path(sys.argv[0]).name} --run-job JOB.json")

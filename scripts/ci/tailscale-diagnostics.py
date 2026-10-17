#!/usr/bin/env python3
"""Disposable hosted diagnostic only. No product qualification or local invocation.

Future command (after separate exact publication/effects approval):
  python3 scripts/ci/tailscale-diagnostics.py --linux-bind
macOS uses no alias mount: the stock alias control is explicitly unsupported.
No logs, canaries, model bodies, paths or subprocess output are published.
"""
import os
import sys

# Admission BEFORE fixture import, scratch creation, compiler or child execution.
if (os.environ.get('GITHUB_ACTIONS') != 'true' or
        os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted' or os.getuid() == 0):
    raise SystemExit('diagnostics require a non-root GitHub-hosted runner')

import json
import re
import signal
import shutil
import stat
import subprocess
import tempfile
import time
from pathlib import Path

BRANCH = 'refs/heads/ci/tailscale-diagnostics-ef9f843-v8'
TOOLS = ('stat', 'head', 'wc')
CELLS = ('cmp_unchanged', 'model_check_rewrite', 'fast_direct', 'strict_delegate', 'real_tool')
MAX_SAMPLES = 3  # Exploratory, sequential, not a benchmark; tool starts perturb all cells.


class ScratchMounted(RuntimeError):
    """Never recurse into an owned root after an unverified unmount."""


def summary_cell(walls, top_level_spawns, inner_spawns):
    """Wall time is end-to-end, not (and cannot be inferred from) spawn count."""
    if not walls or len(walls) > MAX_SAMPLES or any(type(n) is not int or n < 0 for n in walls):
        raise ValueError('invalid bounded wall samples')
    if top_level_spawns != len(walls) or not 0 <= inner_spawns <= 2 * len(walls):
        raise ValueError('invalid spawn attribution')
    return {'samples': len(walls), 'wall_ns_total': sum(walls),
            'top_level_processes': top_level_spawns, 'known_inner_processes': inner_spawns,
            'attribution': 'end_to_end_only'}


def inner_per_sample(cell):
    # FAST_STUB's missing model.valid short-circuits cmp: rewrite forks only
    # .model-ok. Direct/delegate fork cmp; exec replaces the stub, not a fork.
    topology = {'cmp_unchanged': 0, 'model_check_rewrite': 1,
                'fast_direct': 1, 'strict_delegate': 1, 'real_tool': 0}
    return topology[cell]


def status_for_control(available, complete, expected):
    # Missing stock capability is never a passing negative; a broken required
    # fixture/observation is red, not evidence that a mount cannot alias.
    if not available: return 'unsupported'
    if not complete: return 'inconclusive'
    return 'observed' if expected else 'red'


def admitted():
    workspace = Path(os.environ.get('GITHUB_WORKSPACE', ''))
    scratch = Path(os.environ.get('RUNNER_TEMP', ''))
    sha = os.environ.get('GITHUB_SHA', '')
    if (os.environ.get('GITHUB_REF') != BRANCH or not re.fullmatch(r'[0-9a-f]{40}', sha)
            or not workspace.is_absolute() or not scratch.is_absolute()
            or len(str(workspace)) > 512 or len(str(scratch)) > 512):
        raise RuntimeError('untrusted runner coordinates')
    for path in (workspace, scratch):
        if (path.is_symlink() or not path.is_dir() or path.stat().st_uid != os.getuid()
                or path.stat().st_mode & 0o022):
            raise RuntimeError('untrusted runner directory')
    if Path.cwd() != workspace.resolve() or (workspace / 'scripts/ci/tailscale-diagnostics.py').is_symlink():
        raise RuntimeError('untrusted checkout')
    # Fail before effects; child stdout is consumed only as a SHA (never logged).
    sha_actual = subprocess.run(['git', 'rev-parse', 'HEAD'], cwd=workspace,
                                capture_output=True, timeout=5, check=True).stdout.decode('ascii').strip()
    if sha_actual != sha: raise RuntimeError('checkout SHA mismatch')
    return workspace, scratch, sha


def timed(argv, env, cwd, limit=40, suite=False):
    start = time.monotonic_ns()
    # All output discarded, including errors; fixed labels only in evidence.
    with subprocess.Popen(argv, env=env, cwd=cwd, stdin=subprocess.DEVNULL,
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                          start_new_session=True) as p:
        try:
            code = p.wait(timeout=limit - 10 if suite else limit)
        except subprocess.TimeoutExpired:
            if suite:
                # The opt-in fixture TERM handler owns its separately-sessioned
                # process groups. Allow its finally blocks to finish WITHIN the
                # existing 900s bound. Do not reap the suite group leader before
                # the final kill: its PID pins the group's identity.
                os.killpg(p.pid, signal.SIGTERM)
                time.sleep(8)
            try: os.killpg(p.pid, signal.SIGKILL)
            except ProcessLookupError: pass
            p.wait(timeout=2)
            raise RuntimeError('bounded diagnostic child timeout')
    return code, time.monotonic_ns() - start


def microprofile(workspace, root):
    # One deliberately small separate synthetic workload per host. Does NOT
    # infer suite hot-path costs from counts; not a replacement for real fixtures.
    sys.path.insert(0, str(workspace / 'tests'))  # only AFTER admission
    from test_service_uninstall_cli import ADAPTER, install_stub
    control = root / 'micro' / 'control'; control.mkdir(parents=True)
    bin_dir = root / 'micro' / 'bin'; bin_dir.mkdir()
    subject = root / 'micro' / 'subject'; subject.write_bytes(b'fixture\n')
    for tool in TOOLS: install_stub(bin_dir, tool, ADAPTER)
    plain = {'platform': 'Linux', 'fault': '', 'files': {}, 'states': {}, 'fault_label': ''}
    model = control / 'model.json'; model.write_text(json.dumps(plain))
    flag = '-f' if sys.platform == 'darwin' else '-c'
    args = {'stat': [flag, '%d:%i', str(subject)], 'head': ['-n', '1', str(subject)],
            'wc': ['-c', str(subject)]}
    env = {'FIXTURE_ROOT': str(root / 'micro'), 'PATH': '/usr/bin:/bin'}
    results = {}
    for tool in TOOLS:
        argv = [str(bin_dir / tool)] + args[tool]
        groups = {}
        # For each branch: explicit precondition; validate result and retain
        # only wall time and known subprocess topology, never command output.
        for cell in CELLS:
            walls = []
            for sample in range(MAX_SAMPLES):
                if cell == 'strict_delegate':
                    model.write_text(json.dumps(dict(plain, fault='stat-fail' if tool == 'stat' else 'read-fail')))
                elif cell == 'model_check_rewrite':
                    model.write_text(json.dumps(plain))
                    (control / 'model.valid').unlink(missing_ok=True)
                else:
                    model.write_text(json.dumps(plain))
                if cell in ('cmp_unchanged', 'fast_direct', 'strict_delegate'):
                    # Warm ONLY via the real JSON/shape validator. Never seed
                    # model.valid by hand or bypass the adapter's byte oracle.
                    check = bin_dir / '.python' / '.model-ok'
                    if timed([str(check), str(control)], env, root)[0]:
                        raise RuntimeError('model validation control failed')
                command = ({'cmp_unchanged': ['/usr/bin/cmp', '-s', str(model), str(control / 'model.valid')],
                            'real_tool': ['/usr/bin/' + tool] + args[tool]}.get(cell, argv))
                code, ns = timed(command, env, root)
                if (code == 0) != (cell != 'strict_delegate'):
                    raise RuntimeError('microprofile control mismatch')
                walls.append(ns)
            # Exec of the real tool or adapter does not add a process.
            groups[cell] = summary_cell(walls, MAX_SAMPLES, MAX_SAMPLES * inner_per_sample(cell))
        results[tool] = groups
    return results


def mount_control(workspace, root, linux_bind):
    ordinary = root / 'ordinary'; ordinary.mkdir()
    (root / 'link').symlink_to(ordinary, target_is_directory=True)
    alias = root / 'alias'; alias.mkdir()
    mounted = False
    attempted = False
    cross_dir = None
    result = {'alias': 'unsupported', 'cross_device': 'unsupported',
              'inventory': 'inconclusive', 'limit': '4096_entries_1MiB_linux_4096_entries_darwin'}
    try:
        if sys.platform == 'linux':
            # Optional stock tmpfs only: metadata of /dev/shm and a generated
            # owned 0700 directory. Never inspect other entries or mount it.
            shm = Path('/dev/shm')
            if (not shm.is_symlink() and shm.is_dir() and shm.stat().st_uid == 0
                    and stat.S_IMODE(shm.stat().st_mode) == 0o1777):
                cross_dir = Path(tempfile.mkdtemp(prefix='herdr-diagnostic-', dir=shm))
        if sys.platform == 'linux' and linux_bind:
            # Future hosted-only effect requiring its OWN prospective approval.
            # Only the generated alias is a mount target; sudo -n cannot prompt.
            attempted = True  # even a timed-out/failed mount may have taken effect
            code, _ = timed(['sudo', '-n', '/usr/bin/mount', '--bind', str(ordinary), str(alias)],
                            {'PATH': '/usr/bin:/bin'}, root, 10)
            if code: raise RuntimeError('scratch bind mount failed')
            mounted = True
        elif sys.platform != 'darwin':
            raise RuntimeError('unsupported runner OS')
        binary = root / 'observer'
        code, _ = timed(['cc', '-std=c11', '-O0', '-Wall', '-Wextra', '-o', str(binary),
                         str(workspace / 'scripts/ci/tailscale-mount-observe.c')],
                        {'PATH': '/usr/bin:/bin'}, root, 30)
        if code: raise RuntimeError('observer compile failed')
        completed = subprocess.run([str(binary), str(root)] + ([str(cross_dir)] if cross_dir else []), cwd=root,
                                   env={'GITHUB_ACTIONS': 'true', 'RUNNER_ENVIRONMENT': 'github-hosted'},
                                   capture_output=True, timeout=10, check=False)
        # Strict, bounded finite output contract: no arbitrary compiler/probe text.
        if completed.returncode or len(completed.stdout) > 512 or completed.stderr:
            raise RuntimeError('observer refused or exceeded evidence bound')
        line = completed.stdout.decode('ascii').strip()
        match = re.fullmatch(r'ordinary=(0|1) symlink_refused=(0|1) fd_path=(0|1) inventory=(0|1) alias=(0|1) cross=(0|1|2) entries=([0-9]{1,4})', line)
        if not match: raise RuntimeError('observer evidence shape mismatch')
        ordinary_ok, link_ok, fd_ok, inventory_ok, alias_ok, cross_ok, count = map(int, match.groups())
        result['inventory'] = status_for_control(True, bool(inventory_ok), bool(ordinary_ok and link_ok and fd_ok))
        result['entries'] = count
        if result['inventory'] != 'observed': raise RuntimeError('required ordinary control failed')
        if mounted:
            result['alias'] = status_for_control(True, True, bool(alias_ok))
            if result['alias'] != 'observed': raise RuntimeError('required alias control failed')
        if cross_dir:
            result['cross_device'] = status_for_control(True, True, cross_ok == 1)
            if result['cross_device'] != 'observed': raise RuntimeError('required cross-device control failed')
        elif cross_ok != 2: raise RuntimeError('unrequested cross-device observation')
        # Darwin APFS firmlinks and f_mntonname path spelling are not guaranteed
        # to equal the path used to open a file. An fsid match is not a complete
        # mount inventory mapping. No nullfs/stock alias facility is assumed.
        return result
    finally:
        cross_error = False
        if cross_dir is not None:
            # This generated empty directory is independent of the bind target.
            try: cross_dir.rmdir()
            except OSError: cross_error = True
        if attempted:
            try:
                code, _ = timed(['sudo', '-n', '/usr/bin/umount', str(alias)],
                                {'PATH': '/usr/bin:/bin'}, root, 10)
                if code: raise ScratchMounted('scratch umount failed')
            except Exception as exc:
                # Same-device bind mounts defeat Path.is_mount(). Even an
                # uncertain timeout must preserve the whole runner scratch.
                raise ScratchMounted('scratch unmount unverified') from exc
        if cross_error: raise RuntimeError('scratch cross-device cleanup failed')


def suites(workspace, root):
    results = {}
    for name, argv in (('service', [sys.executable, 'tests/test_service_uninstall_cli.py']),
                       ('full', ['/bin/bash', 'tests/test_uninstall.sh'])):
        profile = root / (name + '.json')
        env = {'HOME': str(root), 'PATH': os.environ['PATH'], 'GITHUB_ACTIONS': 'true',
               'RUNNER_ENVIRONMENT': 'github-hosted', 'RUNNER_TEMP': str(root),
               'HERDR_DIAG_PROFILE_FILE': str(profile)}
        timed_out = False
        try:
            code, wall = timed(argv, env, workspace, 900, suite=True)
        except RuntimeError:
            code, wall, timed_out = 124, 0, True
        results[name] = {'exit': code, 'wall_ns': wall, 'profile': 'missing'}
        if profile.is_file():
            if profile.stat().st_size > 32768: raise RuntimeError('profile evidence over limit')
            data = json.loads(profile.read_text())
            cells = data.get('cells', {})
            samples = data.get('cases_sampled', [])
            if (set(data) != {'schema', 'perturbation', 'sample_limit', 'cases_seen', 'cases_sampled', 'cells'}
                    or data.get('schema') != 1 or data.get('sample_limit') != 128
                    or data.get('perturbation') != 'instrumented_wall_includes_fixture_and_tracing'
                    or type(data.get('cases_seen')) is not int or not 0 <= data['cases_seen'] <= 10000
                    or not isinstance(cells, dict) or not isinstance(samples, list) or len(samples) > 128
                    or set(cells) - {'linux_ordinary', 'linux_adverse', 'darwin_ordinary', 'darwin_adverse'}
                    or any(set(row) != {'count', 'wall_ns', 'stub_spawns'} or
                           any(type(value) is not int or not 0 <= value <= 10**15 for value in row.values())
                           for row in cells.values())
                    or any(set(row) != {'case', 'os', 'class', 'wall_ns', 'stub_spawns'} or
                           row['os'] not in ('Linux', 'Darwin') or row['class'] not in ('ordinary', 'adverse') or
                           any(type(row[key]) is not int or not 0 <= row[key] <= 10**15
                               for key in ('case', 'wall_ns', 'stub_spawns')) for row in samples)):
                raise RuntimeError('profile evidence invalid')
            results[name]['profile'] = data
        # If TERM+grace failed, cases are incomplete; do not start the next
        # suite. The job fails red instead of counting a timeout as evidence.
        if timed_out: break
    return results


def main():
    workspace, scratch, sha = admitted()
    if sys.argv[1:] not in ([], ['--linux-bind']): raise RuntimeError('invalid diagnostic arguments')
    if sys.platform == 'darwin' and sys.argv[1:]: raise RuntimeError('Darwin alias facility unsupported')
    output = scratch / 'tailscale-diagnostics'
    if output.is_symlink() or output.exists(): raise RuntimeError('evidence already exists')
    output.mkdir(mode=0o700)
    report = {'schema': 1, 'candidate_sha': sha, 'os': 'Darwin' if sys.platform == 'darwin' else 'Linux',
              'result': 'inconclusive', 'limitations': ['synthetic', 'sampling_perturbs_timing',
              'no_atomic_mount_admission_proof', 'only_first_128_ordinal_cases_sampled',
              'remaining_case_cells_aggregated_inconclusive', 'no_product_qualification']}
    root = Path(tempfile.mkdtemp(prefix='owned-', dir=scratch))
    cleanup = True
    try:
        report['mount'] = mount_control(workspace, root, bool(sys.argv[1:]))
        report['micro'] = microprofile(workspace, root)
        report['fixtures'] = suites(workspace, root)
        if len(report['fixtures']) != 2 or any(row['exit'] or row['profile'] == 'missing' for row in report['fixtures'].values()):
            raise RuntimeError('fixture red or absent evidence')
        report['result'] = 'observed_with_unsupported_controls'
    except ScratchMounted:
        report['result'] = 'red'
        cleanup = False  # same-device bind mount may not satisfy is_mount()
        raise
    except Exception:
        report['result'] = 'red'
        raise
    finally:
        (output / 'summary.json').write_text(json.dumps(report, sort_keys=True) + '\n')
        if cleanup: shutil.rmtree(root)


if __name__ == '__main__':
    try: main()
    except Exception:
        print('hosted diagnostics failed; sanitized summary may be incomplete', file=sys.stderr)
        raise SystemExit(1)

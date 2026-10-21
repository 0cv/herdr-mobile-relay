#!/usr/bin/env python3
"""Real-mount controls for the Linux full uninstall.

Runs the real relay/uninstall.sh, with the real compiled relay (NATIVE_HELPER) as its mount guard, against fixture HOMEs on a
disposable GitHub-hosted Linux runner. The mounts are real bind mounts, made only through scripts/ci/run-uninstall-mount-controls.sh
(sudo mount --bind / umount inside its work directory). Refuses everywhere else; no unit manager, route or network is touched:
systemctl and herdr are inert fixture stubs.
"""
import hashlib, os, pathlib, pty, shutil, signal, stat, subprocess, sys


def refuse(why):
    print('uninstall mount controls refused: ' + why, file=sys.stderr)
    raise SystemExit(2)


if sys.version_info < (3, 11): refuse('Python 3.11 or newer is required')
if not sys.platform.startswith('linux'): refuse('requires a Linux runner')
if os.environ.get('GITHUB_ACTIONS') != 'true' or os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted' or os.geteuid() == 0:
    refuse('requires a disposable non-root GitHub-hosted runner')
REPO = pathlib.Path(__file__).resolve().parent.parent
WORK = pathlib.Path(os.environ.get('HERDR_MOUNT_WORK', ''))
MOUNTCTL = os.environ.get('HERDR_MOUNTCTL', '')
HELPER = os.environ.get('NATIVE_HELPER', '')
if not WORK.is_absolute() or WORK.is_symlink() or not WORK.is_dir() or WORK.resolve() != WORK: refuse('HERDR_MOUNT_WORK is not a real directory')
if not os.path.isabs(MOUNTCTL) or not os.path.isfile(MOUNTCTL): refuse('HERDR_MOUNTCTL is not the mount driver')
if not os.path.isabs(HELPER) or not os.path.isfile(HELPER) or not os.access(HELPER, os.X_OK): refuse('NATIVE_HELPER is not an executable file')
OUTSIDE_BYTES = b'outside bytes must not change\n'
RELEASE_LABEL = 'herdr-mobile-relay.service'
# The unit manager stand-in of tests/test_dev_tailscale.py (inactive, disabled units that exist while their file does), plus an
# optional hook: after the native remover has deleted the main unit, add ONE real mount (the "mount after the native step" control).
SYSTEMCTL = r'''#!/bin/sh
printf '%s\n' "$*" >> "$UNINSTALL_SYSTEMCTL_RECORD"
case "$*" in *is-active*) exit 1 ;; esac
if [ "$2" = show ]; then
    label=$3
    unit="$HOME/.config/systemd/user/$label"
    if [ -f "$unit" ]; then
        load=loaded; enabled=disabled; fragment=$unit
        [ "$label" != herdr-mobile-relay.service ] || : > "$HOOK_STATE.seen"
    else
        load=not-found; enabled=; fragment=
        if [ "$label" = herdr-mobile-relay.service ] && [ -n "$HOOK_SRC" ] && [ -f "$HOOK_STATE.seen" ] && [ ! -e "$HOOK_STATE.done" ]; then
            : > "$HOOK_STATE.done"
            /bin/bash "$HERDR_MOUNTCTL" __mountctl bind "$HOOK_SRC" "$HOOK_DST" || exit 1
        fi
    fi
    case "$*" in
        *--all*) printf 'Id=%s\nLoadState=%s\nActiveState=inactive\nUnitFileState=%s\nFragmentPath=%s\nDropInPaths=\nNeedDaemonReload=no\n' "$label" "$load" "$enabled" "$fragment" ;;
        *) printf 'LoadState=%s\nActiveState=inactive\nUnitFileState=%s\n' "$load" "$enabled" ;;
    esac
fi
exit 0
'''
HERDR = '#!/bin/sh\ntest "$*" = \'plugin uninstall herdr-mobile-relay.events\'\n'


def mountctl(*args):
    subprocess.run(['/bin/bash', MOUNTCTL, '__mountctl', *args], check=True, stdin=subprocess.DEVNULL, timeout=120)


def mounted(path):
    return str(path) in [line.split()[4] for line in pathlib.Path('/proc/self/mountinfo').read_text().splitlines()]


def census():
    # Sanitized evidence of what this runner's mount table is made of: filesystem types and counts only, never a path or a source.
    kinds, points = {}, {}
    for line in pathlib.Path('/proc/self/mountinfo').read_text().splitlines():
        fields = line.split(); kind = fields[fields.index('-') + 1]
        kinds[kind] = kinds.get(kind, 0) + 1; points[fields[4]] = points.get(fields[4], 0) + 1
    text = 'mount census (filesystem types only, no paths): %s; mounts=%d; mountpoints with stacked mounts=%d' % (
        ', '.join('%s=%d' % item for item in sorted(kinds.items())), sum(kinds.values()), sum(1 for n in points.values() if n > 1))
    print(text, flush=True)
    evidence = os.environ.get('HERDR_MOUNT_EVIDENCE')
    if evidence: (pathlib.Path(evidence) / 'census.txt').write_text(text + '\n')


def tree(path, skip=()):
    """Every name, type, device, inode and mode (plus size and digest for files, target for links) below path, the root
    itself included (None if absent); a link is not followed, so a replaced directory, file, link or root is not equal."""
    out = {}
    def visit(q, rel):
        info = q.lstat()
        if stat.S_ISLNK(info.st_mode): out[rel] = ('link', info.st_dev, info.st_ino, stat.S_IMODE(info.st_mode), os.readlink(q))
        elif stat.S_ISDIR(info.st_mode):
            out[rel] = ('dir', info.st_dev, info.st_ino, stat.S_IMODE(info.st_mode))
            for child in sorted(q.iterdir()):
                if not (rel == '.' and child.name in skip): visit(child, rel + '/' + child.name)
        elif stat.S_ISREG(info.st_mode):
            with q.open('rb') as stream: digest = hashlib.file_digest(stream, 'sha256').hexdigest()
            out[rel] = ('file', info.st_dev, info.st_ino, stat.S_IMODE(info.st_mode), info.st_size, digest)
        else: out[rel] = ('other', info.st_mode)
    try: visit(pathlib.Path(path), '.')
    except FileNotFoundError: return None
    return out


class Case:
    count = 0

    def __init__(self):
        Case.count += 1
        self.root = WORK / ('case-%d' % Case.count); self.root.mkdir(mode=0o700)
        self.home = self.root / 'home'; self.relay = self.root / 'relay'; self.bin = self.root / 'bin'
        self.control = self.root / 'control'; self.tmp = self.root / 'tmp'
        for q in (self.home, self.relay, self.bin, self.control, self.tmp): q.mkdir(mode=0o700)
        for name in ('common.sh', 'uninstall.sh', 'uninstall-systemd-user-service.sh'): shutil.copy2(REPO / 'relay' / name, self.relay / name)
        for name, text in (('systemctl', SYSTEMCTL), ('herdr', HERDR)): (self.bin / name).write_text(text); (self.bin / name).chmod(0o700)
        self.release = self.home / '.local/share/herdr-mobile-relay'; self.config = self.home / '.config/herdr-mobile-relay'
        self.cache = self.home / '.cache/herdr-mobile-relay'; self.state = self.home / '.local/state/herdr-mobile-relay'
        self.units = self.home / '.config/systemd/user'; self.outside = self.home / 'outside'
        self.registration = self.state / 'tailscale-cli-registration'; self.coordination = self.state / 'tailscale-cli-coordination'
        for q in (self.release, self.config, self.cache, self.registration, self.coordination, self.units, self.outside):
            q.mkdir(mode=0o700, parents=True)
        for q in (self.release, self.config, self.cache):
            marker = q / '.herdr-mobile-relay-installation'
            marker.write_text('product=herdr-mobile-relay\nroot=%s\n' % q.resolve()); marker.chmod(0o600)
        (self.release / 'current').mkdir(mode=0o700); shutil.copyfile(HELPER, self.release / 'current/herdr-mobile-relay')
        (self.release / 'current/herdr-mobile-relay').chmod(0o700)
        (self.release / 'versions/v1/web').mkdir(parents=True); (self.release / 'versions/v1/web/index.html').write_text('release web\n')
        (self.config / 'notes').write_text('config bytes\n'); (self.cache / 'blob').write_text('cache bytes\n')
        self.env_file = self.config / 'relay.env'
        self.env_file.write_text('HERDR_RELAY_TRANSPORT=tailscale-cli\nHERDR_TAILSCALE_CLI_STATE_ROOT=%s\nHERDR_TAILSCALE_CLI_COORDINATION_ROOT=%s\n'
                                 % (self.registration, self.coordination)); self.env_file.chmod(0o600)
        for q, names in ((self.registration, ('registration.json', 'journal', 'route')), (self.coordination, ('reservation',))):
            for name in names: (q / name).write_text('protected ' + name + '\n'); (q / name).chmod(0o600)
        self.unit = self.units / RELEASE_LABEL
        self.unit.write_text('[Service]\nEnvironment="HERDR_RELAY_ENV=%s"\n' % self.env_file); self.unit.chmod(0o600)
        self.record = self.control / 'systemctl-record'; self.hook = ('', ''); self.tmpdir = self.tmp
        self.mounts = []

    def protected(self, skip=()):
        return {name: tree(path, skip) for name, path in (('release', self.release), ('config', self.config), ('cache', self.cache),
                ('outside', self.outside), ('units', self.units), ('state', self.state))}

    def bind(self, source, target):
        mountctl('bind', str(source), str(target)); self.mounts.append(target)
        assert mounted(target), 'the bind mount did not appear in the mount table'

    def mount(self, root_name, kind):
        """One real bind mount aimed at a root: over it, nested beneath it on the same device, or the root bound elsewhere."""
        root = getattr(self, root_name)
        if kind == 'over':
            source = self.outside / (root_name + '-source'); shutil.copytree(root, source, symlinks=True)
            (source / 'outside-bytes').write_bytes(OUTSIDE_BYTES); self.bind(source, root)
        elif kind == 'nested':
            source = self.outside / (root_name + '-nested-source'); source.mkdir()
            (source / 'outside-bytes').write_bytes(OUTSIDE_BYTES); (root / 'nested').mkdir(); self.bind(source, root / 'nested')
        else:
            alias = self.outside / (root_name + '-alias'); alias.mkdir(); self.bind(root, alias)

    def link_release(self):
        """The releases root is a symlink to an outside directory whose binary would record that it was executed."""
        target = self.outside / 'release-target'; self.release.rename(target); self.release.symlink_to(target)
        (target / 'current/herdr-mobile-relay').write_text('#!/bin/sh\n: > "%s"\nexit 0\n' % (self.control / 'executed'))
        return target

    def hook_after_native(self, root_name):
        root = getattr(self, root_name); source = self.outside / (root_name + '-hook-source'); source.mkdir()
        (source / 'outside-bytes').write_bytes(OUTSIDE_BYTES); (root / 'nested').mkdir()
        self.hook = (str(source), str(root / 'nested'))

    def run(self):
        env = {'HOME': str(self.home), 'PATH': '%s:/usr/bin:/bin' % self.bin, 'LC_ALL': 'C', 'TMPDIR': str(self.tmpdir),
               'HERDR_RELAY_ENV': str(self.env_file), 'HERDR_PLUGIN_CONFIG_DIR': str(self.config), 'HERDR_RELEASE_ROOT': str(self.release),
               'XDG_CACHE_HOME': str(self.cache.parent), 'XDG_STATE_HOME': str(self.state.parent),
               'HERDR_RELAY_BIN_DIR': str(self.home / '.local/bin'), 'UNINSTALL_SYSTEMCTL_RECORD': str(self.record),
               'HOOK_STATE': str(self.control / 'hook'), 'HOOK_SRC': self.hook[0], 'HOOK_DST': self.hook[1],
               'HERDR_MOUNTCTL': MOUNTCTL, 'HERDR_MOUNT_WORK': str(WORK),
               'GITHUB_ACTIONS': 'true', 'RUNNER_ENVIRONMENT': 'github-hosted'}
        master, slave = pty.openpty()
        process = subprocess.Popen(['/bin/bash', str(self.relay / 'uninstall.sh')], env=env, cwd=self.root, stdin=slave,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
        os.close(slave)
        try:
            os.write(master, b'n\ny\ny\n')  # do not remove the route, explicitly leave it, continue
            output = process.communicate(timeout=180)[0].decode('utf-8', 'replace')
        finally:
            os.close(master)
            try: os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError: pass
        return process.returncode, output

    def finish(self):
        for target in reversed(self.mounts + ([pathlib.Path(self.hook[1])] if self.hook[1] else [])):
            if mounted(target): mountctl('unbind', str(target))
        for target in self.mounts + ([pathlib.Path(self.hook[1])] if self.hook[1] else []):
            assert not mounted(target), 'a fixture mount was left behind'

    def staged(self):
        return list(self.tmp.glob('herdr-uninstall-guard.*'))

    def systemctl_calls(self):
        return self.record.read_text().splitlines() if self.record.exists() else []

    def refused(self, rc, output, before, skip=()):
        """Refused by the guard before any route, native or shared effect: nothing at all changed."""
        assert rc == 1 and 'mount confinement' in output, output
        assert 'persists independently' not in output and 'has been uninstalled.' not in output, output
        assert self.protected(skip) == before, 'a refused uninstall changed a root, a definition, the journal or outside bytes'
        assert all(call.startswith('--user show ') or 'is-active' in call for call in self.systemctl_calls()), self.systemctl_calls()
        assert not (self.control / 'hook.done').exists() and not self.staged(), 'staged guard copy left behind'


def main():
    census()
    assert not [p for p in pathlib.Path('/proc/self/mountinfo').read_text().splitlines() if str(WORK) in p.split()[4]], 'stale mounts'
    # 1. Positive: all three roots, real binary, real guard, route and journal retained.
    case = Case(); before = case.protected()
    try:
        rc, output = case.run()
        assert rc == 0 and 'has been uninstalled.' in output and 'Leaving persistent route' in output, output
        assert not any(q.exists() or q.is_symlink() for q in (case.release, case.config, case.cache, case.unit))
        assert not case.staged(), 'staged guard copy left behind'
        after = case.protected()
        assert after['state'] == before['state'] and after['outside'] == before['outside'], 'route journal or outside bytes changed'
    finally: case.finish()
    # 2. For EACH root: a bind mount at the root, a nested same-device bind beneath it, and the root bind-mounted elsewhere.
    for root_name in ('release', 'config', 'cache'):
        for kind in ('over', 'nested', 'elsewhere'):
            case = Case()
            try:
                case.mount(root_name, kind); before = case.protected()
                rc, output = case.run(); case.refused(rc, output, before)
                assert all(mounted(t) for t in case.mounts), 'the refused run unmounted a fixture mount'
                print('refused before any effect: %s root, %s' % (root_name, kind), flush=True)
            finally: case.finish()
    # 3. A mount added after the native step: the recheck before the first destructive step refuses (releases), and the
    #    one before the last (cache) refuses with the releases root and its binary already gone (the staged private copy).
    for root_name in ('release', 'cache'):
        case = Case()
        try:
            case.hook_after_native(root_name); before = case.protected(skip=('nested',))
            rc, output = case.run()
            assert (case.control / 'hook.done').exists() and mounted(case.hook[1]), 'the post-native mount was not made'
            assert rc == 1 and 'mount confinement' in output and 'has been uninstalled.' not in output, output
            assert not case.unit.exists(), 'native removal did not precede the mount'
            after = case.protected(skip=('nested',))
            assert after['state'] == before['state'] and after['outside'] == before['outside'], 'journal or outside bytes changed'
            if root_name == 'release':
                assert all(after[name] == before[name] for name in ('release', 'config', 'cache')), 'a refused uninstall changed a root'
            else: assert not case.release.exists() and not case.config.exists() and after['cache'] == before['cache']
            assert not case.staged(), 'staged guard copy left behind'
            print('refused by the per-seam recheck after the native step: %s root' % root_name, flush=True)
        finally: case.finish()
    # 3b. The staged private guard copy must survive the deletion of every root, so a temp directory inside a root is refused
    #     before anything is made; and a symlinked releases root (never traversed) cannot supply or execute the guard.
    for root_name, via_link in (('release', False), ('cache', False), ('release', True)):
        case = Case()
        try:
            inside = getattr(case, root_name) / 'tmp-in-root'; inside.mkdir(mode=0o700); case.tmpdir = inside
            if via_link: case.tmpdir = case.outside / 'tmp-link'; case.tmpdir.symlink_to(inside)  # a symlinked TMPDIR is followed
            before = case.protected()
            rc, output = case.run(); case.refused(rc, output, before)
            assert not list(inside.iterdir()), 'a guard copy was staged inside a root'
            print('refused before any effect: %stemp directory inside the %s root' % ('symlinked ' if via_link else '', root_name), flush=True)
        finally: case.finish()
    # 3c. A TMPDIR that is a bind-mount alias of a root, or of a subtree of one, has a physical name outside every root, so a path
    #     comparison cannot refuse it: only the proof that precedes any write can. Nothing may be created in, or removed from, the
    #     aliased directory (its own mtime and ctime change if an entry is added or removed, even if the content ends up equal).
    for root_name, aliased in (('release', 'root'), ('release', 'subtree'), ('cache', 'root'), ('cache', 'subtree')):
        case = Case()
        try:
            root = getattr(case, root_name); target = root
            if aliased == 'subtree': target = root / 'subtree'; target.mkdir(mode=0o700)
            alias = case.outside / (root_name + '-tmp-alias'); alias.mkdir(mode=0o700)
            case.bind(target, alias); case.tmpdir = alias
            before = case.protected(); stamp = os.lstat(target); stamp = (stamp.st_mtime_ns, stamp.st_ctime_ns)
            rc, output = case.run(); case.refused(rc, output, before)
            now = os.lstat(target)
            assert (now.st_mtime_ns, now.st_ctime_ns) == stamp, 'a guard copy was staged through, or removed from, a bind-mounted alias'
            assert not list(target.glob('herdr-uninstall-guard.*')), 'a guard copy was left in a root'
            print('refused before any write: temp directory bind-mounted from the %s %s' % (root_name, aliased), flush=True)
        finally: case.finish()
    case = Case()
    try:
        case.link_release(); before = case.protected()
        rc, output = case.run(); case.refused(rc, output, before)
        assert not (case.control / 'executed').exists(), 'the executable behind a symlinked releases root was run'
        print('refused before any effect: symlinked releases root', flush=True)
    finally: case.finish()
    # 4. Absent and link-only roots: nothing to prove, the link alone is removed, and neither blocks the other roots.
    case = Case(); shutil.rmtree(case.cache); before = case.protected()
    try:
        rc, output = case.run()
        assert rc == 0 and 'has been uninstalled.' in output and not case.cache.exists(), output
        assert case.protected()['outside'] == before['outside'] and not case.staged()
    finally: case.finish()
    case = Case(); shutil.rmtree(case.cache); target = case.outside / 'cache-target'; target.mkdir()
    (target / 'bytes').write_bytes(OUTSIDE_BYTES); case.cache.symlink_to(target); before = tree(target)
    try:
        rc, output = case.run()
        assert rc == 0 and 'has been uninstalled.' in output and 'Removed cache symlink' in output, output
        assert not case.cache.exists() and not case.cache.is_symlink() and tree(target) == before, 'link-only removal touched its target'
    finally: case.finish()
    print('PASS real-mount uninstall controls: positive, 9 root x mount-kind refusals, 2 post-native rechecks, temp directory in a root, 4 bind-mounted temp directory aliases, symlinked releases root, absent and link-only roots')


main()

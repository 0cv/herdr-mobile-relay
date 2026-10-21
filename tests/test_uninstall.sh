#!/usr/bin/env bash
set -euo pipefail
# Genuine admission BEFORE discovery, roots, imports or children. Never forge it.
if [ "${GITHUB_ACTIONS:-}" != true ] || [ "${RUNNER_ENVIRONMENT:-}" != github-hosted ] || [ "$(id -u)" -eq 0 ]; then
    echo 'uninstall fixtures require a genuine disposable non-root GitHub-hosted runner' >&2; exit 1
fi
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd -P)"
python3 - "$REPO_DIR" <<'PY'
import os, sys
if sys.version_info < (3, 11): raise SystemExit('uninstall fixtures require Python >=3.11')
import hashlib, json, pty, shutil, signal, stat, subprocess, tempfile, time
from pathlib import Path
sys.path.insert(0, str(Path(sys.argv[1]) / 'tests'))
# Reuse the reviewed finite inert controller DATA/source-copy patterns. This
# executes only in the later admitted fixture phase, never at source authoring.
from test_service_uninstall_cli import ADAPTER, INVOCATIONS, Fixture, bounded_communicate, contain, drain_pty, failure_states, fast_stub_equivalence_control, install_stub, snapshot, spawn_report, spawn_tally
REPO = Path(sys.argv[1]); TIMEOUT = 40
# On a Linux host the Linux-model cases run the REAL mount guard: the compiled relay is installed as current/herdr-mobile-relay.
LINUX_HOST = sys.platform.startswith('linux'); HELPER = os.environ.get('NATIVE_HELPER', '')
if LINUX_HOST and not (os.path.isabs(HELPER) and os.path.isfile(HELPER) and os.access(HELPER, os.X_OK)):
    raise SystemExit('uninstall fixtures on Linux require NATIVE_HELPER: the compiled relay, which provides the real mount guard')
# A MODEL of the Linux mount guard, NOT the real one. A macOS host cannot run the compiled Linux relay, so its Linux-model cases (and
# every Darwin-model case on any host, where the guard must never run) install this recording script as current/herdr-mobile-relay.
# It records each call (argv, in call order) and answers allow, deny or fail per call from control/guard-verdicts ("N VERDICT" lines).
GUARD_MODEL = r'''#!/bin/sh
control="$FIXTURE_ROOT/control"
n=0; [ ! -f "$control/guard-calls" ] || read -r n < "$control/guard-calls"
n=$((n + 1)); printf '%s\n' "$n" > "$control/guard-calls"
list=""; for a in "$@"; do list="$list${list:+, }\"$a\""; done
staged=0; for d in "$TMPDIR"/herdr-uninstall-guard.*; do [ ! -e "$d" ] || staged=$((staged + 1)); done
case "$0" in "$TMPDIR"/herdr-uninstall-guard.*/*) from=staged ;; *) from=installed ;; esac
printf '{"op": "guard", "n": %s, "args": [%s], "staged": %s, "from": "%s"}\n' "$n" "$list" "$staged" "$from" >> "$control/events.jsonl"
verdict=allow
if [ -f "$control/guard-verdicts" ]; then
    while read -r index word; do [ "$index" != "$n" ] || verdict=$word; done < "$control/guard-verdicts"
fi
case "$verdict" in
    allow) exit 0 ;;
    deny) echo 'herdr-mobile-relay: uninstall mount guard refused' >&2; exit 1 ;;
    *) exit 70 ;;
esac
'''
PREFIX = r'''import json, os, pathlib, sys, time
root = pathlib.Path(os.environ['FIXTURE_ROOT']); control = root / 'control'
model_path = control / 'model.json'; model = json.loads(model_path.read_text())
name = pathlib.Path(sys.argv[0]).name; args = sys.argv[1:]
def record(op, **extra):
    with (control / 'events.jsonl').open('a') as stream: stream.write(json.dumps(dict(op=op, **extra)) + '\n')
def store(): model_path.write_text(json.dumps(model))
def quiet(): return not model.get('unknown_seen') and all(s['state'] in ('inactive', 'absent') and not s['enabled'] for s in model['states'].values())
# Record the actual delegate argv, then run the copied remover unchanged with
# the same /bin/bash as the caller. No helper replacement or manager shortcut.
if name == 'bash':
    remover = root / 'relay' / ('uninstall-systemd-user-service.sh' if model['platform'] == 'Linux' else 'uninstall-service.sh')
    record('native-delegate', args=args)
    if args not in ([str(remover)], [str(remover), '--cli-route-disposition=retained'], [str(remover), '--cli-route-disposition=removed']):
        record('forbidden', tool=name); raise SystemExit(97)
    os.execv('/bin/bash', ['/bin/bash'] + args)
# The staged private guard copy lives in this fixture's own tmp/; only the script's own cleanup removes it.
if name == 'rm' and len(args) == 3 and args[:2] == ['-rf', '--'] and pathlib.Path(args[2]).parent == root / 'tmp' and pathlib.Path(args[2]).name.startswith('herdr-uninstall-guard.'):
    if model.get('cleanup_fail'): record('guard-cleanup-refused'); raise SystemExit(1)  # fixture: this one rm fails
    record('guard-cleanup'); os.execv('/bin/rm', ['rm'] + args)
# Explicit finite paths; no filesystem existence -> controller activity inference.
shared = model['shared']
if name in ('rm', 'chmod', 'herdr'):
    batch = None
    if name == 'herdr' and args == ['plugin', 'uninstall', 'herdr-mobile-relay.events']: batch = 'registration'
    if name == 'chmod' and len(args) >= 2 and args[0] == 'u+w':
        release = pathlib.Path(shared['release'])
        if all(pathlib.Path(p) == release or release in pathlib.Path(p).parents for p in args[1:]):
            if all(pathlib.Path(p).is_dir() and not pathlib.Path(p).is_symlink() for p in args[1:]):
                batch = 'release-mode'
    if name == 'rm' and len(args) == 2 and args[0] in ('-f', '-rf'):
        batch = next((key for key, value in shared.items() if value == args[1]), None)
    if batch:
        record('shared-attempt', batch=batch, quiet=quiet())
        if not quiet(): record('forbidden', tool=name); raise SystemExit(97)
        if batch == model.get('batch_failure'): raise SystemExit(1)
        if name == 'herdr': raise SystemExit(0)
        os.execv('/bin/' + name, [name] + args)
# A fixture-only post-native flag is armed by checked definition removal, or
# the fourth exact legacy observation in the all-absent positive. Metadata
# counters are independent. No production observer is duplicated here.
if not model.get('native_done') and all(not pathlib.Path(q).exists() for q in model['files'].values()) and all(s['queries'] >= 4 for s in model['states'].values()):
    model['native_done'] = True; store()
label = None
if name == 'systemctl' and len(args) == 7 and args[:2] == ['--user', 'show']: label = args[2]
if name == 'launchctl' and len(args) == 2 and args[0] == 'print' and '/' in args[1][4:]: label = args[1].split('/', 2)[2]
if label in model['states'] and model.get('native_done'):
    gates = model.setdefault('gates', {}); gates[label] = gates.get(label, 0) + 1
    record('shared-gate', label=label, at=gates[label])
    if label == model.get('late_label') and gates[label] == model.get('late_at'):
        if model['late_kind'] == 'active':
            model['states'][label].update(state='active' if model['platform'] == 'Linux' else 'loaded', enabled=True)
        elif model['late_kind'] == 'fifo':
            os.mkfifo(model['fifo_path'], 0o600)  # an object the real guard refuses, added after native removal
        else:
            model.update(fault='query-fail', fault_label=label, unknown_seen=True)
    store()
'''
# Native definition mutation remains the original strict quiet/disabled seam.
FULL_ADAPTER = PREFIX + ADAPTER.replace("if op == 'disable': s['enabled'] = False", "if op == 'disable' and not fault(label, 'disable-noop'): s['enabled'] = False").replace(
    "pathlib.Path(args[2]).unlink(missing_ok=True); raise SystemExit(0)",
    "pathlib.Path(args[2]).unlink(missing_ok=True)\n    if all(not pathlib.Path(q).exists() for q in model['files'].values()): model['native_done'] = True; save()\n    raise SystemExit(0)")
# What FULL_ADAPTER's PREFIX dereferences on EVERY call, stat, head and wc included: model['shared'] and
# model['states'] unconditionally (the gate's `label in model['states']`), and model['files'].values() and each
# state's s['queries'] >= 4 in the native_done arming block until native_done is set. The fast stat/head/wc
# stubs cache only a model of this shape and delegate for any other, so the adapter fails as before instead of
# the stub running the real tool. The LAST clause is that arming block's own condition, negated: a model the
# prefix would arm (no model file exists, every state queried 4 times, native_done unset) is never cached, so
# the stub delegates it and the adapter arms and STORES native_done on this very call, exactly as for every
# other tool. A cached model cannot become armable behind the stub's back: model files leave existence only
# through the adapter's rm, which stores native_done itself once the last one is gone, and a state's queries
# only grow in an adapter's observe(), which rewrites model.json (so the cached copy no longer matches and the
# model is revalidated). One expression of `model`, baked into the validator by install_stub.
# The fast_stub_equivalence_control below compares the final model.json of both implementations as well.
ARMING_DONE = ("(model.get('native_done') or not (all(not pathlib.Path(q).exists() for q in model['files'].values())"
               " and all(s['queries'] >= 4 for s in model['states'].values())))")
FULL_MODEL_SHAPE = ("isinstance(model.get('files'), dict) and all(isinstance(path, str) for path in model['files'].values())"
                    " and isinstance(model.get('shared'), dict)"
                    " and isinstance(model.get('states'), dict)"
                    " and all(isinstance(state, dict) and isinstance(state.get('queries'), (int, float)) for state in model['states'].values())"
                    " and " + ARMING_DONE)

class FullFixture(Fixture):
    def __init__(self, parent, platform, present=(0, 1), mode='cloudflare', fault='', fault_index=0):
        super().__init__(parent, platform, present, fault, fault_index, mode)
        src = REPO / 'relay/uninstall.sh'; dst = self.relay / 'uninstall.sh'; shutil.copy2(src, dst)
        assert hashlib.sha256(src.read_bytes()).digest() == hashlib.sha256(dst.read_bytes()).digest()
        assert stat.S_IMODE(src.stat().st_mode) == stat.S_IMODE(dst.stat().st_mode)
        self.release = self.home / 'custom/releases-root'; self.config = self.home / 'custom/config/herdr-mobile-relay'
        self.cache = self.home / 'custom/cache/herdr-mobile-relay'; self.link = self.home / '.local/bin/herdr-mobile-relay'
        for q in (self.release, self.config, self.cache, self.link.parent, self.root / 'tmp'): q.mkdir(parents=True, mode=0o700)
        for q in (self.release, self.config, self.cache):
            marker = q / '.herdr-mobile-relay-installation'; marker.write_text('product=herdr-mobile-relay\nroot=' + str(q) + '\n'); marker.chmod(0o600)
        current = self.release / 'current'; current.mkdir(mode=0o700)
        if platform == 'Linux' and LINUX_HOST: shutil.copyfile(HELPER, current / 'herdr-mobile-relay')
        else: (current / 'herdr-mobile-relay').write_text(GUARD_MODEL)
        (current / 'herdr-mobile-relay').chmod(0o700)
        self.link.symlink_to(current / 'herdr-mobile-relay')
        sealed = self.release / 'releases/sealed/web'; sealed.mkdir(parents=True)
        (sealed / 'index.html').write_text('sealed release\n'); sealed.chmod(0o555); sealed.parent.chmod(0o555)
        self.env_file.replace(self.config / 'relay.env'); self.env_file = self.config / 'relay.env'
        if mode == 'gateway':
            with self.env_file.open('a') as stream: stream.write('HERDR_GATEWAY_URL=https://gateway.example.test\n')
        self.environment.update(HERDR_RELAY_ENV=str(self.env_file), HERDR_PLUGIN_CONFIG_DIR=str(self.config),
                                HERDR_RELEASE_ROOT=str(self.release), XDG_CACHE_HOME=str(self.cache.parent), TMPDIR=str(self.root / 'tmp'))
        self.model['shared'] = dict(release=str(self.release), config=str(self.config), cache=str(self.cache), link=str(self.link))
        for i in present: self.write_definition(i, self.env_file)
        self.save()
        for name in ('systemctl', 'launchctl', 'uname', 'plutil', 'rm', 'sleep', 'wc', 'head', 'stat', 'herdr', 'chmod', 'bash'):
            install_stub(self.bin, name, FULL_ADAPTER, model_shape=FULL_MODEL_SHAPE)
        self.source = self.home / 'source-checkout'; self.source.mkdir(mode=0o700)
        (self.source / '.env').write_text("GH_TOKEN='private-source-canary'\n")
        self.roots = (self.release, self.config, self.cache, self.link)
        self.before = tuple(snapshot(q) for q in self.roots)
        self.protected_before = (snapshot(self.private), snapshot(self.source))
        self.private_no_route = tuple((q.name, snapshot(q)) for q in sorted(self.private.iterdir()) if q.name != 'route')

    def invoke(self, answer=b'y\ny\n', requested_signal=None, tty=False):
        started = time.monotonic(); spawns_before = spawn_tally(self.control)
        master = slave = None
        if tty: master, slave = pty.openpty()
        p = subprocess.Popen(['/bin/bash', '-x', str(self.relay / 'uninstall.sh')], env=self.environment,
            stdin=slave if tty else subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
        if slave is not None: os.close(slave)
        stop_drain = drain_pty(master) if master is not None else None
        bystander = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(120)'], start_new_session=True)
        try:
            if tty: os.write(master, answer)
            if requested_signal:
                p.stdin.write(answer); p.stdin.flush()
                deadline = time.monotonic() + TIMEOUT
                while not (self.control / 'ready').exists():
                    if p.poll() is not None or time.monotonic() > deadline: raise AssertionError('signal handshake missing')
                    time.sleep(.01)
                p.send_signal(requested_signal); (self.control / 'release').touch()
                p.stdin.close(); p.stdin = None
            output = bounded_communicate(p, None if tty or requested_signal else answer)[0]
            assert bystander.poll() is None
            assert all(canary not in output for canary in (b'private-source-canary', b'fixture-saved-canary', b'fixture-ambient-canary', b'fixture-ambient-github-canary'))
            events = self.events(); assert not any(e['op'] == 'forbidden' for e in events), events
            if self.model['platform'] == 'Darwin': assert not any(e['op'] in ('guard', 'guard-cleanup') for e in events), 'the Linux guard ran on Darwin'
            assert self.protected_before[1] == snapshot(self.source)
            assert not any(e.get('op') in ('restart', 'bootstrap', 'kickstart', 'enable') for e in events)
            return p.returncode, output, events
        finally:
            INVOCATIONS[0] += 1
            print('full uninstall fixture invocation %d took %.1fs; %s' % (
                INVOCATIONS[0], time.monotonic() - started,
                spawn_report(spawns_before, spawn_tally(self.control))), file=sys.stderr, flush=True)
            failure_states(p)
            if stop_drain is not None: stop_drain()
            if master is not None: os.close(master)
            contain(p); contain(bystander, bystander=True)

    def retained(self, output, events):
        assert self.before == tuple(snapshot(q) for q in self.roots)
        assert not any(e['op'] == 'shared-attempt' for e in events)
        assert b'has been uninstalled.' not in output and b'Continue with file removal anyway' not in output
        assert self.protected_before == (snapshot(self.private), snapshot(self.source))

# The fast stat/head/wc stubs delegate to FULL_ADAPTER for every fault; this proves they agree with it.
fast_stub_equivalence_control(FULL_ADAPTER, FULL_MODEL_SHAPE)
SUB = '__private-uninstall-mount-roots'
def guard_args(events): return [e['args'] for e in events if e['op'] == 'guard']
# On Linux the real guard needs the fixture HOME on the root filesystem; RUNNER_TEMP is, whatever /tmp may be mounted as.
with tempfile.TemporaryDirectory(prefix='herdr-full-uninstall-', dir=os.environ.get('RUNNER_TEMP') if LINUX_HOST else None) as temporary:
    parent = Path(temporary).resolve()
    for platform in ('Linux', 'Darwin'):
        # F001: real /bin/bash (stock 3.2 on Darwin), nounset enabled in the
        # copied caller, and exactly ZERO delegate arguments for non-CLI modes.
        for mode in ('cloudflare', 'gateway'):
            for present in ((0, 1), (), (0,), (1,)):
                f = FullFixture(parent, platform, present, mode=mode); status, out, events = f.invoke()
                assert status == 0 and b'has been uninstalled.' in out
                assert not any(q.exists() or q.is_symlink() for q in f.roots)
                assert f.protected_before == (snapshot(f.private), snapshot(f.source))
                assert any(e['op'] == 'shared-attempt' for e in events)
                assert [e['args'][1:] for e in events if e['op'] == 'native-delegate'] == [[]]
        for index in (0, 1):
            for fault in ('query-fail', 'malformed', 'stop-noop', 'disable-fail', 'disable-noop',
                          'stop-fail' if platform == 'Linux' else 'bootout-fail',
                          'contradictory' if platform == 'Linux' else 'domain-fail',
                          'missing' if platform == 'Linux' else 'disabled-fail',
                          'unknown' if platform == 'Linux' else 'domain-malformed',
                          'duplicate' if platform == 'Linux' else 'disabled-malformed'):
                f = FullFixture(parent, platform, fault=fault, fault_index=index)
                status, out, events = f.invoke(b'y\ny\ny\n')
                assert status != 0; f.retained(out, events)
                assert f.definitions[index].exists()
                if fault not in ('domain-fail', 'domain-malformed', 'disabled-fail', 'disabled-malformed'):
                    assert not f.definitions[1-index].exists()  # native partial progress in either direction
            for kind in ('active', 'unknown'):
                for gate in range(1, 9):
                    f = FullFixture(parent, platform)
                    f.model.update(late_label=f.labels[index], late_at=gate, late_kind=kind); f.save()
                    status, out, events = f.invoke()
                    assert status != 0 and b'has been uninstalled.' not in out
                    assert all(e.get('quiet') is True for e in events if e['op'] == 'shared-attempt')
                    if gate <= 2: f.retained(out, events)
                    else: assert b'Earlier verified shared-file batches completed' in out
            seams = ('systemd',) if platform == 'Linux' else ('domain', 'label', 'disabled')
            for seam in seams:
                for text in ('\x00', 'X' * (4097 if seam == 'systemd' else 65537)):
                    f = FullFixture(parent, platform, fault='lifecycle-output', fault_index=index)
                    # The launchd domain listing is read by STATUS alone (its text is discarded), so its refusal is a failing query (status 1), as in test_service_uninstall_cli.py.
                    f.model.update(lifecycle_seam=seam, lifecycle_text=text, lifecycle_status=1 if seam == 'domain' else 0); f.save()
                    status, out, events = f.invoke(); assert status != 0; f.retained(out, events)
                    if seam != 'domain': continue
                    # The same NUL or over-64 KiB listing with status 0 (the hosted macOS 15 domain prints 89,044 bytes) no longer refuses.
                    f = FullFixture(parent, platform, fault='lifecycle-output', fault_index=index)
                    f.model.update(lifecycle_seam=seam, lifecycle_text=text, lifecycle_status=0); f.save()
                    status, out, events = f.invoke()
                    assert status == 0 and b'has been uninstalled.' in out and any(e['op'] == 'lifecycle-capture' for e in events)
            f = FullFixture(parent, platform, fault='lifecycle-output', fault_index=index)
            f.model.update(lifecycle_seam='systemd' if platform == 'Linux' else 'label',
                           lifecycle_text='LoadState=loaded\nActiveState=inactive\nUnitFileState=disabled' if platform == 'Linux' else 'controller unavailable',
                           lifecycle_status=1); f.save()
            status, out, events = f.invoke(); assert status != 0; f.retained(out, events)
            # Absent file plus a cached job is NOT absence. Linux metadata refuses;
            # launchd proceeds only if actual disable/bootout and observations agree.
            f = FullFixture(parent, platform, present=())
            f.model['states'][f.labels[index]].update(state='active' if platform == 'Linux' else 'loaded', enabled=True)
            f.model.update(fault='stop-noop', fault_label=f.labels[index]); f.save()
            status, out, events = f.invoke(); assert status != 0; f.retained(out, events)
        for shape in ('missing', 'symlink', 'nonzero'):
            f = FullFixture(parent, platform); q = f.relay / ('uninstall-systemd-user-service.sh' if platform == 'Linux' else 'uninstall-service.sh')
            q.unlink()
            if shape == 'symlink': q.symlink_to(f.relay / 'common.sh')
            if shape == 'nonzero': q.write_text('#!/bin/bash\nexit 23\n'); q.chmod(0o700)
            status, out, events = f.invoke(); assert status == (23 if shape == 'nonzero' else 1); f.retained(out, events)
        for code in (130, 143):
            f = FullFixture(parent, platform)
            q = f.relay / ('uninstall-systemd-user-service.sh' if platform == 'Linux' else 'uninstall-service.sh')
            q.write_text('#!/bin/bash\nexit ' + str(code) + '\n'); q.chmod(0o700)
            status, out, events = f.invoke(); assert status == code; f.retained(out, events)
        f = FullFixture(parent, platform); f.model['platform'] = 'Unsupported'; f.save()
        status, out, events = f.invoke(); assert status == 1; f.retained(out, events)
        f = FullFixture(parent, platform)
        different = f.root / 'different.env'; different.write_text('HERDR_RELAY_TRANSPORT=cloudflare\n'); different.chmod(0o600)
        f.write_definition(1, different)
        status, out, events = f.invoke(); assert status == 1; f.retained(out, events)
        f = FullFixture(parent, platform); status, out, events = f.invoke(b'n\n')
        assert status == 0 and b'Cancelled.' in out; f.retained(out, events)
        for sig in (signal.SIGINT, signal.SIGTERM):
            f = FullFixture(parent, platform, fault='pause-stop'); status, out, events = f.invoke(requested_signal=sig)
            assert status == (130 if sig == signal.SIGINT else 143); f.retained(out, events)
        # Refuse both a nested and an active-release outside hard link BEFORE
        # route disposition or native removal, on each hosted platform. Mode,
        # bytes and inode of the outside name (not just its path) must survive.
        for mode in ('cloudflare', 'tailscale-cli'):
            for inside in ('nested', 'active'):
                f = FullFixture(parent, platform, mode=mode)
                outside = f.home / 'outside-release-file'
                outside.write_bytes(b'outside inode must not change\n')
                outside.chmod(0o400)
                linked = (f.release / 'releases/sealed/web/index.html' if inside == 'nested'
                          else f.release / 'current/herdr-mobile-relay')
                if inside == 'nested': linked.parent.chmod(0o755)
                linked.unlink(); os.link(outside, linked)
                if inside == 'nested': linked.parent.chmod(0o555)
                f.before = tuple(snapshot(q) for q in f.roots)
                before_stat = outside.stat()
                before_bytes = outside.read_bytes()
                status, out, events = f.invoke(b'y\ny\n', tty=(mode == 'tailscale-cli'))
                after_stat = outside.stat()
                assert status == 1 and b'REFUSING to remove releases with multiply-linked files' in out
                assert (before_stat.st_dev, before_stat.st_ino, stat.S_IMODE(before_stat.st_mode), before_stat.st_nlink) == (
                    after_stat.st_dev, after_stat.st_ino, stat.S_IMODE(after_stat.st_mode), after_stat.st_nlink)
                assert outside.read_bytes() == before_bytes and linked.samefile(outside)
                f.retained(out, events)
                assert all(q.exists() for q in f.definitions)
                assert not any(e['op'] in ('unpublish', 'native-delegate', 'stop', 'disable', 'bootout',
                                          'remove', 'shared-attempt') for e in events)
                assert f.private_no_route == tuple((q.name, snapshot(q)) for q in sorted(f.private.iterdir()) if q.name != 'route')
        for batch in ('link', 'release-mode', 'release', 'config', 'cache', 'registration'):
            f = FullFixture(parent, platform); f.model['batch_failure'] = batch; f.save()
            status, out, events = f.invoke(); assert status != 0 and b'has been uninstalled.' not in out
            assert [e['batch'] for e in events if e['op'] == 'shared-attempt'][-1] == batch
            assert b'partial shared effects are possible' in out
        for bad_root in (parent / 'outside-home', None):
            f = FullFixture(parent, platform)
            target = bad_root or f.home / 'unrelated'; target.mkdir(exist_ok=True)
            f.environment['HERDR_RELEASE_ROOT'] = str(target)
            status, out, events = f.invoke(); assert status == 1; f.retained(out, events)
        f = FullFixture(parent, platform)
        ancestry = f.home / 'unsearchable'; ancestry.mkdir(mode=0o700)
        release = ancestry / 'owned-release'; release.mkdir(mode=0o700)
        marker = release / '.herdr-mobile-relay-installation'
        marker.write_text('product=herdr-mobile-relay\nroot=' + str(release) + '\n'); marker.chmod(0o600)
        before = snapshot(ancestry); f.environment['HERDR_RELEASE_ROOT'] = str(release)
        ancestry.chmod(0o600)
        try: status, out, events = f.invoke()
        finally: ancestry.chmod(0o700)
        assert status == 1 and snapshot(ancestry) == before; f.retained(out, events)
        assert not any(e['op'] in ('stop', 'disable', 'bootout', 'remove') for e in events)
        # Link-only directory removal leaves its target, including sentinel, intact.
        f = FullFixture(parent, platform); other = f.home / 'bystander-cache'; other.mkdir(); (other / 'keep').write_text('keep\n')
        shutil.rmtree(f.cache); f.cache.symlink_to(other); protected_link_target = snapshot(other)
        status, out, events = f.invoke(); assert status == 0 and snapshot(other) == protected_link_target
        if platform == 'Linux' and not LINUX_HOST:  # all three roots are preflighted (the link by its own location, not traversed); only directories are rechecked
            assert guard_args(events) == [[SUB, str(f.release), str(f.config), str(f.cache)]] * 2 + [[SUB, str(f.release)], [SUB, str(f.release)], [SUB, str(f.config)]]
        for content in ('HERDR_RELEASE_ROOT=/should-not-be-imported\n', 'HOME=/should-not-be-imported\n', 'GH_TOKEN=$(touch /should-not-execute)\n'):
            f = FullFixture(parent, platform)
            with f.env_file.open('a') as stream: stream.write(content)
            f.before = tuple(snapshot(q) for q in f.roots)
            status, out, events = f.invoke(); assert status == 1; f.retained(out, events)
        for choice, expected in ((b'n\ny\ny\n', 0), (b'y\ny\n', 0), (b'n\nn\n', 1)):
            f = FullFixture(parent, platform, mode='tailscale-cli')
            # The exact-route adapter is inert. No actual Tailscale access.
            status, out, events = f.invoke(choice, tty=True); assert status == expected
            route_calls = [e for e in events if e['op'] == 'unpublish']
            assert len(route_calls) == (1 if choice.startswith(b'y') else 0)
            delegate_args = [e['args'][1:] for e in events if e['op'] == 'native-delegate']
            disposition = 'removed' if choice.startswith(b'y') else 'retained'
            assert delegate_args == ([] if expected else [['--cli-route-disposition=' + disposition]])
            if expected: f.retained(out, events)
            assert f.private_no_route == tuple((q.name, snapshot(q)) for q in sorted(f.private.iterdir()) if q.name != 'route')
            assert not any(e['op'] in ('release-backend-port', 'off', 'reset', 'drain') for e in events)
        for key in ('HERDR_TAILSCALE_CLI_STATE_ROOT', 'HERDR_TAILSCALE_CLI_COORDINATION_ROOT'):
            f = FullFixture(parent, platform, mode='tailscale-cli')
            with f.env_file.open('a') as stream: stream.write(key + '="' + str(f.config / 'registration') + '"\n')
            f.before = tuple(snapshot(q) for q in f.roots)
            status, out, events = f.invoke(b'y\ny\n', tty=True); assert status == 1; f.retained(out, events)
            assert not any(e['op'] in ('unpublish', 'stop', 'disable', 'bootout', 'remove') for e in events)
        # F003: the actual CLI coordination default remains under HOME even
        # with non-default XDG_STATE_HOME. Refuse BEFORE route/native/shared
        # effects, for existing AND absent definitions and either route choice.
        for present in ((0, 1), ()):
            for choice in (b'n\ny\ny\n', b'y\ny\n'):
                f = FullFixture(parent, platform, present, mode='tailscale-cli')
                config = f.home / '.local/state/herdr-mobile-relay'; config.parent.mkdir(parents=True, mode=0o700)
                f.config.rename(config); f.config = config; f.env_file = config / 'relay.env'
                (config / '.herdr-mobile-relay-installation').write_text('product=herdr-mobile-relay\nroot=' + str(config) + '\n')
                alternate = f.home / 'alternate-state'
                registration = alternate / 'herdr-mobile-relay/tailscale-cli-registration'
                wrong_coordination = alternate / 'herdr-mobile-relay/tailscale-cli-coordination'
                coordination = config / 'tailscale-cli-coordination'
                for root in (registration, wrong_coordination, coordination):
                    root.mkdir(parents=True, mode=0o700)
                    for name in ('journal', 'reservation'):
                        q = root / name; q.write_text('protected default-root fixture\n'); q.chmod(0o600)
                f.environment.update(HERDR_RELAY_ENV=str(f.env_file), HERDR_PLUGIN_CONFIG_DIR=str(config), XDG_STATE_HOME=str(alternate))
                assert 'HERDR_TAILSCALE_CLI_COORDINATION_ROOT' not in f.environment and 'HERDR_TAILSCALE_CLI_COORDINATION_ROOT' not in f.env_file.read_text()
                assert 'HERDR_TAILSCALE_CLI_STATE_ROOT' not in f.environment and 'HERDR_TAILSCALE_CLI_STATE_ROOT' not in f.env_file.read_text()
                f.model['shared']['config'] = str(config)
                for i in present: f.write_definition(i, f.env_file)
                f.save(); f.roots = (f.release, config, f.cache, f.link)
                f.before = tuple(snapshot(q) for q in f.roots)
                protected = (coordination, registration, wrong_coordination, *f.definitions)
                before = tuple(snapshot(q) for q in protected)
                status, out, events = f.invoke(choice, tty=True)
                assert status == 1 and b'CLI registration root overlaps uninstall target' in out
                f.retained(out, events); assert before == tuple(snapshot(q) for q in protected)
                assert not any(e['op'] in ('unpublish', 'native-delegate', 'stop', 'disable', 'bootout', 'remove', 'shared-attempt') for e in events)
        f = FullFixture(parent, platform, mode='tailscale-cli'); f.model['route_status'] = 1; f.save()
        status, out, events = f.invoke(b'y\ny\n', tty=True); assert status == 1; f.retained(out, events)
        assert not any(e['op'] in ('stop', 'disable', 'bootout', 'remove') for e in events)
    # Linux mount-confinement wiring. On a Linux host the guard is the real compiled relay and a refusal is induced with a real object
    # (a FIFO); on a macOS host it is the recording MODEL, which also proves the exact order and arguments of every call.
    def attempts(events): return [e['batch'] for e in events if e['op'] == 'shared-attempt']
    def no_effects(events): assert not any(e['op'] in ('unpublish', 'native-delegate', 'stop', 'disable', 'remove', 'shared-attempt') for e in events), events
    f = FullFixture(parent, 'Linux', mode='tailscale-cli'); status, out, events = f.invoke(b'y\ny\n', tty=True); assert status == 0
    r, c, k = str(f.release), str(f.config), str(f.cache)
    expected = [('guard', None), ('guard', None), ('unpublish', None), ('native-delegate', None), ('shared-attempt', 'link'), ('guard', None),
                ('shared-attempt', 'release-mode'), ('guard', None), ('shared-attempt', 'release'), ('guard', None),
                ('shared-attempt', 'config'), ('guard', None), ('shared-attempt', 'cache'), ('shared-attempt', 'registration')]
    if LINUX_HOST: expected = [x for x in expected if x[0] != 'guard']; assert not guard_args(events)
    else:
        assert guard_args(events) == [[SUB, r, c, k], [SUB, r, c, k], [SUB, r], [SUB, r], [SUB, c], [SUB, k]]
        # Nothing is staged before the first proof, which runs the installed relay in place; every later call runs the staged copy.
        assert [(e['from'], e['staged']) for e in events if e['op'] == 'guard'] == [('installed', 0)] + [('staged', 1)] * 5
    assert [(e['op'], e.get('batch')) for e in events if e['op'] in ('guard', 'unpublish', 'native-delegate', 'shared-attempt')] == expected
    for shape in (('fifo', 'not-executable') if LINUX_HOST else ('deny', 'fail', 'staged-deny', 'not-executable')):
        routed = shape in ('deny', 'staged-deny', 'fifo')
        f = FullFixture(parent, 'Linux', mode='tailscale-cli' if routed else 'cloudflare')
        if shape == 'not-executable': (f.release / 'current/herdr-mobile-relay').chmod(0o600); f.before = tuple(snapshot(q) for q in f.roots)
        elif shape == 'fifo': os.mkfifo(f.release / 'refused-fifo', 0o600)
        else: (f.control / 'guard-verdicts').write_text(('2 deny' if shape == 'staged-deny' else '1 ' + shape) + '\n')
        status, out, events = f.invoke(b'y\ny\n', tty=routed)
        (f.release / 'refused-fifo').unlink(missing_ok=True)
        assert status != 0 and b'mount confinement' in out and b'persists independently' not in out
        f.retained(out, events); no_effects(events); assert all(q.exists() for q in f.definitions)
        assert guard_args(events) == ([] if LINUX_HOST or shape == 'not-executable' else [[SUB, str(f.release), str(f.config), str(f.cache)]] * (2 if shape == 'staged-deny' else 1))
        assert not list((f.root / 'tmp').glob('herdr-uninstall-guard.*')), 'a refusal left a staged guard copy'
    # The recheck before each destructive step: recheck N follows the (N+1)-th shared quiet gate. The model's guard calls are
    # 1 = preflight, installed relay in place; 2 = preflight, staged copy; then 3 = before the release chmod, 4 = before rm of releases,
    # 5 = config, 6 = cache (N = call - 1); nothing at or after the refused step runs.
    for call, which, done in ((2, 'release', ['link']), (3, 'release', ['link', 'release-mode']),
                              (4, 'config', ['link', 'release-mode', 'release']), (5, 'cache', ['link', 'release-mode', 'release', 'config'])):
        f = FullFixture(parent, 'Linux'); root = getattr(f, which)
        if LINUX_HOST: f.model.update(late_label=f.labels[0], late_at=call + 1, late_kind='fifo', fifo_path=str(root / 'refused-fifo')); f.save()
        else: (f.control / 'guard-verdicts').write_text('%d deny\n' % (call + 1))
        status, out, events = f.invoke()
        (root / 'refused-fifo').unlink(missing_ok=True)
        assert status != 0 and b'mount confinement' in out and b'has been uninstalled.' not in out
        assert attempts(events) == done, events
        if not LINUX_HOST: assert len(guard_args(events)) == call + 1 and guard_args(events)[-1] == [SUB, str(root)]
        refused = ('release', 'config', 'cache').index(which)
        for i, name in enumerate(('release', 'config', 'cache')):
            q = getattr(f, name)
            if i < refused: assert not q.exists()
            elif call == 3 and i == refused: assert q.is_dir() and (q / '.herdr-mobile-relay-installation').is_file()  # its directory modes changed
            else: assert snapshot(q) == f.before[i]
    f = FullFixture(parent, 'Linux'); shutil.rmtree(f.cache)  # an absent root has nothing to prove
    status, out, events = f.invoke(); assert status == 0 and b'has been uninstalled.' in out and not f.cache.exists()
    if not LINUX_HOST: assert guard_args(events) == [[SUB, str(f.release), str(f.config), str(f.cache)]] * 2 + [[SUB, str(f.release)], [SUB, str(f.release)], [SUB, str(f.config)]]
    # A symlinked releases root is removed as a link only and never traversed, so it cannot supply (or execute) the guard: refuse
    # with no effects, and never run the outside executable it points at.
    f = FullFixture(parent, 'Linux'); moved = f.home / 'bystander-release'; f.release.rename(moved)
    (moved / 'current/herdr-mobile-relay').write_text('#!/bin/sh\n: > "$FIXTURE_ROOT/control/executed"\nexit 0\n')
    f.release.symlink_to(moved); f.before = tuple(snapshot(q) for q in f.roots); bystander = snapshot(moved)
    status, out, events = f.invoke()
    assert status != 0 and b'mount confinement' in out and not (f.control / 'executed').exists()
    f.retained(out, events); no_effects(events); assert snapshot(moved) == bystander and not guard_args(events) and all(q.exists() for q in f.definitions)
    # The staged guard copy must survive the deletion of every root: a temp directory inside a selected root is refused before anything is made.
    # A TMPDIR that is a symlink into a root is followed (physically) and refused the same way.
    for inside, via_link in (('release', False), ('config', False), ('release', True)):
        f = FullFixture(parent, 'Linux'); where = getattr(f, inside) / 'tmp-in-root'; where.mkdir(mode=0o700)
        if via_link: (f.home / 'tmp-link').symlink_to(where)
        f.environment['TMPDIR'] = str(f.home / 'tmp-link' if via_link else where); f.before = tuple(snapshot(q) for q in f.roots)
        status, out, events = f.invoke()
        assert status != 0 and b'mount confinement' in out and not list(where.iterdir())
        f.retained(out, events); no_effects(events); assert not guard_args(events) and all(q.exists() for q in f.definitions)
    # A staged copy that cannot be removed is reported and fails an otherwise successful uninstall (the stub refuses that one rm).
    f = FullFixture(parent, 'Linux'); f.model['cleanup_fail'] = True; f.save()
    status, out, events = f.invoke(); leaked = list((f.root / 'tmp').glob('herdr-uninstall-guard.*'))
    assert status != 0 and b'could not be removed' in out and len(leaked) == 1 and any(e['op'] == 'guard-cleanup-refused' for e in events), out
    shutil.rmtree(leaked[0])
print('PASS actual full-uninstall fail-closed SOURCE controls (execution requires separate admission)')
PY

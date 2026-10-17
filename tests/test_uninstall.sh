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
import atexit, hashlib, json, pty, shutil, signal, stat, subprocess, tempfile, time
from pathlib import Path
sys.path.insert(0, str(Path(sys.argv[1]) / 'tests'))
# Reuse the reviewed finite inert controller DATA/source-copy patterns. This
# executes only in the later admitted fixture phase, never at source authoring.
from test_service_uninstall_cli import ADAPTER, INVOCATIONS, Fixture, bounded_communicate, fast_stub_equivalence_control, install_stub, snapshot, spawn_report, spawn_tally, profile_record, profile_flush, install_profile_termination, spawn_owned_session, forget_owned_session
install_profile_termination()
atexit.register(profile_flush)
REPO = Path(sys.argv[1]); TIMEOUT = 40
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
# Explicit finite paths; no filesystem existence -> controller activity inference.
shared = model['shared']
if name in ('rm', 'chmod', 'herdr'):
    batch = None
    if name == 'herdr' and args == ['plugin', 'uninstall', 'herdr-mobile-relay.events']: batch = 'registration'
    if name == 'chmod' and args == ['-R', 'u+w', shared['release']]: batch = 'release-mode'
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
# the stub running the real tool. One expression of `model`, baked into the validator by install_stub.
FULL_MODEL_SHAPE = ("isinstance(model.get('files'), dict) and all(isinstance(path, str) for path in model['files'].values())"
                    " and isinstance(model.get('shared'), dict)"
                    " and isinstance(model.get('states'), dict)"
                    " and all(isinstance(state, dict) and isinstance(state.get('queries'), (int, float)) for state in model['states'].values())")

class FullFixture(Fixture):
    def __init__(self, parent, platform, present=(0, 1), mode='cloudflare', fault='', fault_index=0):
        super().__init__(parent, platform, present, fault, fault_index, mode)
        src = REPO / 'relay/uninstall.sh'; dst = self.relay / 'uninstall.sh'; shutil.copy2(src, dst)
        assert hashlib.sha256(src.read_bytes()).digest() == hashlib.sha256(dst.read_bytes()).digest()
        assert stat.S_IMODE(src.stat().st_mode) == stat.S_IMODE(dst.stat().st_mode)
        self.release = self.home / 'custom/releases-root'; self.config = self.home / 'custom/config/herdr-mobile-relay'
        self.cache = self.home / 'custom/cache/herdr-mobile-relay'; self.link = self.home / '.local/bin/herdr-mobile-relay'
        for q in (self.release, self.config, self.cache, self.link.parent): q.mkdir(parents=True, mode=0o700)
        for q in (self.release, self.config, self.cache):
            marker = q / '.herdr-mobile-relay-installation'; marker.write_text('product=herdr-mobile-relay\nroot=' + str(q) + '\n'); marker.chmod(0o600)
        current = self.release / 'current'; current.mkdir(mode=0o700)
        (current / 'herdr-mobile-relay').write_text('inert binary\n'); (current / 'herdr-mobile-relay').chmod(0o700)
        self.link.symlink_to(current / 'herdr-mobile-relay')
        sealed = self.release / 'releases/sealed/web'; sealed.mkdir(parents=True)
        (sealed / 'index.html').write_text('sealed release\n'); sealed.chmod(0o555); sealed.parent.chmod(0o555)
        self.env_file.replace(self.config / 'relay.env'); self.env_file = self.config / 'relay.env'
        if mode == 'gateway':
            with self.env_file.open('a') as stream: stream.write('HERDR_GATEWAY_URL=https://gateway.example.test\n')
        self.environment.update(HERDR_RELAY_ENV=str(self.env_file), HERDR_PLUGIN_CONFIG_DIR=str(self.config),
                                HERDR_RELEASE_ROOT=str(self.release), XDG_CACHE_HOME=str(self.cache.parent))
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
        p = spawn_owned_session(['/bin/bash', '-x', str(self.relay / 'uninstall.sh')], env=self.environment,
            stdin=slave if tty else subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        if slave is not None: os.close(slave)
        bystander = spawn_owned_session([sys.executable, '-c', 'import time; time.sleep(120)'])
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
            assert self.protected_before[1] == snapshot(self.source)
            assert not any(e.get('op') in ('restart', 'bootstrap', 'kickstart', 'enable') for e in events)
            return p.returncode, output, events
        finally:
            INVOCATIONS[0] += 1
            spawns_after = spawn_tally(self.control)
            profile_record(self.model['platform'], self.model['fault'], int((time.monotonic() - started) * 1e9), spawns_before, spawns_after)
            print('full uninstall fixture invocation %d took %.1fs; %s' % (
                INVOCATIONS[0], time.monotonic() - started,
                spawn_report(spawns_before, spawns_after)), file=sys.stderr, flush=True)
            for child in (p, bystander):
                try: os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError: pass
                child.wait(timeout=2)
                forget_owned_session(child)
            if master is not None: os.close(master)

    def retained(self, output, events):
        assert self.before == tuple(snapshot(q) for q in self.roots)
        assert not any(e['op'] == 'shared-attempt' for e in events)
        assert b'has been uninstalled.' not in output and b'Continue with file removal anyway' not in output
        assert self.protected_before == (snapshot(self.private), snapshot(self.source))

# The fast stat/head/wc stubs delegate to FULL_ADAPTER for every fault; this proves they agree with it.
fast_stub_equivalence_control(FULL_ADAPTER, FULL_MODEL_SHAPE)
with tempfile.TemporaryDirectory(prefix='herdr-full-uninstall-') as temporary:
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
                    f.model.update(lifecycle_seam=seam, lifecycle_text=text, lifecycle_status=0); f.save()
                    status, out, events = f.invoke(); assert status != 0; f.retained(out, events)
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
print('PASS actual full-uninstall fail-closed SOURCE controls (execution requires separate admission)')
PY

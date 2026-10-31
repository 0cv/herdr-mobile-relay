import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { bindEmulatorChild, emulatorBindingSha256, parseEmulatorLifetime, readEmulatorFile, sampleHostEmulator } from '../support/android-emulator-lifetime';
import { repositoryPath } from '../support/paths';
import { command } from '../support/process';

export const androidEmulatorLifetimeTests: Array<[string, () => Promise<void>]> = [];
const test = (name: string, body: () => Promise<void>) => androidEmulatorLifetimeTests.push([`Android emulator lifetime ${name}`, body]);
const boot = '12345678-1234-1234-1234-123456789abc';
const ticks = '18446744073709551610';
function procStat(pid: number, state = 'S', startTicks = ticks): string {
  return `${pid} (a name ) with (parentheses)) ${state} ${Array(18).fill('0').join(' ')} ${startTicks} 0 0\n`;
}
async function fixture() {
  const root = await mkdtemp(join(tmpdir(), 'emulator-receipt-unit-'));
  const file = join(root, 'receipt.json');
  const values = new Map<string, string>([
    ['/proc/71/stat', procStat(71)], ['/proc/sys/kernel/random/boot_id', boot],
  ]);
  const io = { platform: 'linux', read: async (path: string, limit: number, signal: AbortSignal) => {
    if (values.has(path)) {
      const target = join(root, path.replaceAll('/', '_'));
      await writeFile(target, values.get(path)!);
      return readEmulatorFile(target, limit, signal);
    }
    return readEmulatorFile(path, limit, signal);
  }, readlink: async () => '/private/SDK/qemu-system-x86_64' };
  const binding = await bindEmulatorChild(71, io);
  const receipt = { schema: 1, binding, bindingSha256: emulatorBindingSha256(binding) };
  await writeFile(file, JSON.stringify(receipt), { mode: 0o600 });
  return { root, file, values, io, receipt };
}
for (const [name, code, signal, script] of [
  ['zero', 0, null, 'process.exit(0)'],
  ['nonzero', 7, null, 'process.exit(7)'],
  ['code143', 143, null, 'process.exit(143)'],
  ['native SIGTERM', null, 'SIGTERM', 'process.kill(process.pid,"SIGTERM")'],
] as const) test(`pinned Node direct child ${name}`, async () => {
  const root = await mkdtemp(join(tmpdir(), 'emulator-native-unit-'));
  try {
    assert.equal((await command('node', ['--version'])).stdout.trim(), 'v26.13.0');
    const file = join(root, 'receipt.json'), log = join(root, 'emulator.log');
    const helper = repositoryPath('tests/mobile/support/android-emulator-lifetime.ts');
    const run = await command('node', [helper, file, log, 'node', '-e',
      `console.log('RAW_CANARY '+process.pid+' '+process.ppid);setTimeout(()=>{${script}},30)`]);
    assert.equal(run.stdout, ''); assert.equal(run.stderr, '');
    const text = await readFile(file, 'utf8');
    const receipt = parseEmulatorLifetime(text);
    assert.ok(receipt?.exit);
    assert.equal(receipt.exit.code, code); assert.equal(receipt.exit.signal, signal);
    const logged = /RAW_CANARY (\d+) (\d+)/u.exec(await readFile(log, 'utf8'))!;
    assert.equal(receipt.binding.childPid, Number(logged[1]));
    assert.notEqual(receipt.binding.childPid, Number(logged[2]));
    assert.equal(receipt.binding.status, process.platform === 'linux' ? 'bound' : 'unsupported-os');
    assert.ok(Buffer.byteLength(text) < 2048);
    assert.doesNotMatch(text, /RAW_CANARY|argv|SDK|emulator\.log|comm|environment/u);
    assert.equal((await stat(file)).mode & 0o777, 0o600);
  } finally { await rm(root, { recursive: true, force: true }); }
});
test('exit before birth publication retains native event and queued latest record', async () => {
  const root = await mkdtemp(join(tmpdir(), 'emulator-early-unit-'));
  try {
    const file = join(root, 'receipt.json'), log = join(root, 'emulator.log');
    const helper = pathToFileURL(repositoryPath('tests/mobile/support/android-emulator-lifetime.ts')).href;
    const source = `import {observeEmulator} from ${JSON.stringify(helper)};
      await observeEmulator(${JSON.stringify(file)},${JSON.stringify(log)},'node',['-e','process.exit(7)'],{
        platform:'linux',read:async(path)=>{await new Promise(r=>setTimeout(r,150));
        return path.endsWith('boot_id')?${JSON.stringify(boot)}:path.match(/\\/proc\\/(\\d+)/)[1]+' (child) S '+Array(18).fill('0').join(' ')+' ${ticks}';},
        readlink:async()=>'/sdk/emulator'});`;
    const run = await command('node', ['--input-type=module', '-e', source]);
    assert.equal(run.stderr, '');
    const receipt = parseEmulatorLifetime(await readFile(file, 'utf8'))!;
    assert.equal(receipt.exit?.code, 7);
    assert.equal(receipt.binding.processKind, 'unknown');
    assert.equal(receipt.binding.birth, null);
    assert.equal(receipt.binding.status, 'exit-before-binding');
    assert.equal(receipt.bindingSha256, emulatorBindingSha256(receipt.binding));
  } finally { await rm(root, { recursive: true, force: true }); }
});
test('spawn error is not a native exit', async () => {
  const root = await mkdtemp(join(tmpdir(), 'emulator-spawn-unit-'));
  try {
    const file = join(root, 'receipt.json');
    const run = await command('node', [repositoryPath('tests/mobile/support/android-emulator-lifetime.ts'), file,
      join(root, 'log'), join(root, 'NO_EXECUTABLE_CANARY')]);
    assert.equal(run.stderr, '');
    const text = await readFile(file, 'utf8');
    const receipt = parseEmulatorLifetime(text)!;
    assert.equal(receipt.binding.status, 'spawn-error');
    assert.equal(receipt.binding.childPid, null); assert.equal(receipt.exit, undefined);
    assert.doesNotMatch(text, /CANARY|errorBody|path/u);
  } finally { await rm(root, { recursive: true, force: true }); }
});
for (const variant of ['alive', 'gone', 'zombie', 'exited', 'pid-reused', 'boot-mismatch', 'unreadable', 'malformed', 'byte-limit', 'wrong-pid'] as const) {
  test(`Linux birth-bound checkpoint ${variant}`, async () => {
    const f = await fixture();
    try {
      if (variant === 'gone') f.io.read = async (path, limit, signal) => {
        if (path === '/proc/71/stat') throw Object.assign(new Error('PRIVATE_CANARY'), { code: 'ENOENT' });
        return readEmulatorFile(path === '/proc/sys/kernel/random/boot_id' ? join(f.root, '_proc_sys_kernel_random_boot_id') : path, limit, signal);
      };
      if (variant === 'unreadable') {
        const read = f.io.read;
        f.io.read = async (path, limit, signal) => {
          if (path === '/proc/71/stat') throw Object.assign(new Error('PRIVATE_CANARY'), { code: 'EACCES' });
          return read(path, limit, signal);
        };
      }
      if (variant === 'zombie' || variant === 'exited') f.values.set('/proc/71/stat', procStat(71, variant === 'zombie' ? 'Z' : 'X'));
      if (variant === 'pid-reused') f.values.set('/proc/71/stat', procStat(71, 'S', '99'));
      if (variant === 'boot-mismatch') f.values.set('/proc/sys/kernel/random/boot_id', 'ffffffff-ffff-ffff-ffff-ffffffffffff');
      if (variant === 'malformed') f.values.set('/proc/71/stat', '71 (truncated) S 0 0');
      if (variant === 'byte-limit') f.values.set('/proc/71/stat', 'x'.repeat(4097));
      const settled = Date.now();
      const snapshot = await sampleHostEmulator(f.file, variant === 'wrong-pid' ? '72' : '71', settled, 250, f.io);
      assert.equal(snapshot.hostSettledAtMs, settled);
      assert.ok(snapshot.sampleStartedAtMs >= settled && snapshot.sampleEndedAtMs >= snapshot.sampleStartedAtMs);
      assert.equal(snapshot.state, variant === 'alive' ? 'alive' : ['gone', 'zombie', 'exited'].includes(variant) ? 'gone' : 'unknown');
      const reason = { alive: 'same-birth', gone: 'absent', zombie: 'zombie', exited: 'exited', 'pid-reused': 'pid-reused',
        'boot-mismatch': 'boot-mismatch', unreadable: 'io-error', malformed: 'malformed', 'byte-limit': 'byte-limit', 'wrong-pid': 'invalid-receipt' }[variant];
      assert.equal(snapshot.reason, reason);
      if (variant === 'alive') assert.equal(snapshot.bindingSha256, f.receipt.bindingSha256);
      assert.doesNotMatch(JSON.stringify(snapshot), /PRIVATE_CANARY|SDK|argv|\/proc/u);
    } finally { await rm(f.root, { recursive: true, force: true }); }
  });
}
test('launcher identity and absent native callback do not claim QEMU lifetime', async () => {
  const f = await fixture();
  try {
    f.io.readlink = async () => '/sdk/emulator';
    const binding = await bindEmulatorChild(71, f.io);
    const receipt = { schema: 1, binding, bindingSha256: emulatorBindingSha256(binding) };
    await writeFile(f.file, JSON.stringify(receipt));
    const before = await sampleHostEmulator(f.file, '71', Date.now(), 250, f.io);
    assert.equal(before.state, 'alive'); assert.equal(before.processKind, 'launcher'); assert.equal(before.nativeExit, undefined);
    assert.equal(before.nativeExitCoverage, 'unknown');
    assert.equal(binding.birth?.startTicks, ticks);
    await writeFile(f.file, JSON.stringify({ ...receipt, exit: { observedAtMs: Date.now(), code: null, signal: 'SIGKILL' } }));
    const after = await sampleHostEmulator(f.file, '71', Date.now(), 250, f.io);
    assert.equal(after.bindingSha256, before.bindingSha256);
    assert.equal(after.state, 'alive'); assert.equal(after.nativeExit?.signal, 'SIGKILL');
    assert.equal(after.nativeExitCoverage, 'observed');
    f.io.readlink = async () => 'x'.repeat(4097);
    assert.equal((await bindEmulatorChild(71, f.io)).processKind, 'unknown');
    f.io.readlink = async () => { throw new Error('PRIVATE_CANARY'); };
    assert.equal((await bindEmulatorChild(71, f.io)).processKind, 'unknown');
    assert.equal((await bindEmulatorChild(71, { ...f.io, platform: 'darwin' })).status, 'unsupported-os');
  } finally { await rm(f.root, { recursive: true, force: true }); }
});
test('unavailable birth keeps genuine child PID but cannot establish kernel liveness', async () => {
  const f = await fixture();
  try {
    const binding = await bindEmulatorChild(71, { ...f.io, read: async () => {
      throw Object.assign(new Error('PRIVATE_CANARY'), { code: 'ENOENT' });
    } });
    assert.equal(binding.childPid, 71); assert.equal(binding.birth, null); assert.equal(binding.status, 'missing');
    await writeFile(f.file, JSON.stringify({ schema: 1, binding, bindingSha256: emulatorBindingSha256(binding),
      exit: { observedAtMs: Date.now(), code: 7, signal: null } }));
    const snapshot = await sampleHostEmulator(f.file, '71', Date.now(), 250, f.io);
    assert.equal(snapshot.state, 'unknown'); assert.equal(snapshot.reason, 'missing');
    assert.equal(snapshot.nativeExit?.code, 7); assert.equal(snapshot.nativeExitCoverage, 'observed');
  } finally { await rm(f.root, { recursive: true, force: true }); }
});
test('closed bounded receipts reject malicious fields and truncated bytes', async () => {
  const f = await fixture();
  try {
    for (const receipt of [ { ...f.receipt, args: ['PRIVATE_CANARY'] }, { ...f.receipt, bindingSha256: '0'.repeat(64) },
      { ...f.receipt, binding: { ...f.receipt.binding, comm: 'PRIVATE_CANARY' } },
      { ...f.receipt, exit: { code: 143, signal: 'SIGTERM', observedAtMs: Date.now() } },
      { ...f.receipt, exit: { code: null, signal: 'PRIVATE_CANARY', observedAtMs: Date.now() } } ]) {
      await writeFile(f.file, JSON.stringify(receipt));
      const snapshot = await sampleHostEmulator(f.file, '71', Date.now(), 250, f.io);
      assert.equal(snapshot.reason, 'invalid-receipt'); assert.doesNotMatch(JSON.stringify(snapshot), /PRIVATE_CANARY/u);
    }
    for (const [text, reason] of [['{', 'invalid-receipt'], ['x'.repeat(2049), 'byte-limit']] as const) {
      await writeFile(f.file, text);
      assert.equal((await sampleHostEmulator(f.file, '71', Date.now(), 250, f.io)).reason, reason);
    }
  } finally { await rm(f.root, { recursive: true, force: true }); }
});
test('exhausted budget performs no IO and late IO cannot mutate timeout snapshot', async () => {
  const f = await fixture();
  try {
    let calls = 0;
    const io = { ...f.io, read: async () => { calls++; await new Promise(r => setTimeout(r, 40)); return JSON.stringify(f.receipt); } };
    assert.equal((await sampleHostEmulator(f.file, '71', Date.now(), 0, io)).reason, 'probe-timeout');
    assert.equal(calls, 0);
    const result = await sampleHostEmulator(f.file, '71', Date.now(), 5, io);
    const retained = JSON.stringify(result);
    assert.equal(result.reason, 'probe-timeout');
    await new Promise(r => setTimeout(r, 60));
    assert.equal(JSON.stringify(result), retained); assert.equal(calls, 1);
    const fifo = join(f.root, 'fifo');
    assert.equal(spawnSync('mkfifo', [fifo]).status, 0);
    assert.equal((await sampleHostEmulator(fifo, '71', Date.now(), 100)).reason, 'io-error');
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

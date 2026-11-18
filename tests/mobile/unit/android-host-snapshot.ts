import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, readdir, realpath, rm, stat, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { repositoryPath } from '../support/paths';
import { command } from '../support/process';

export const androidHostSnapshotTests: Array<[string, () => Promise<void>]> = [];
const test = (name: string, body: () => Promise<void>) => androidHostSnapshotTests.push([`Android host snapshot ${name}`, body]);
const helper = repositoryPath('tests/mobile/support/android-host-snapshot.ts');
const secret = 'A'.repeat(43);

async function fixture() {
  const root = await mkdtemp(join(tmpdir(), 'android-host-snapshot-'));
  const bin = join(root, 'bin'), sdk = join(root, 'sdk'), proc = join(root, 'proc'), output = join(root, 'output');
  await Promise.all([mkdir(bin), mkdir(join(sdk, 'platform-tools'), { recursive: true }), mkdir(join(proc, 'pressure'), { recursive: true })]);
  const trace = join(root, 'trace');
  await writeFile(join(bin, 'adb'), `#!/bin/sh
printf '%s\\n' "$*" >> "${trace}"
case "$1" in
  version) printf 'Android Debug Bridge version 1.0.41\\nVersion 37.0.1-15733141\\nInstalled as /sdk/platform-tools/adb\\n' ;;
  devices) printf 'List of devices attached\\nemulator-5554          device product:sdk_gphone64_x86_64 transport_id:3\\n' ;;
esac
`, { mode: 0o755 });
  await symlink(join(bin, 'adb'), join(sdk, 'platform-tools', 'adb'));
  await writeFile(join(proc, 'meminfo'), 'MemTotal:       16374236 kB\nMemFree:          120000 kB\nMemAvailable:    9000000 kB\nSwapTotal:       4194300 kB\nSwapFree:        4194300 kB\n');
  await writeFile(join(proc, 'loadavg'), '3.91 2.10 1.20 5/900 7000\n');
  for (const kind of ['cpu', 'memory', 'io']) {
    await writeFile(join(proc, 'pressure', kind), 'some avg10=12.50 avg60=8.00 avg300=2.00 total=100\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n');
  }
  const env = { ...process.env, PATH: `${bin}:/usr/bin:/bin`, ANDROID_HOME: sdk };
  return { root, bin, proc, output, trace, env };
}

async function addServer(proc: string, pid: number, argv: string[], startTicks: string) {
  await mkdir(join(proc, String(pid)));
  await writeFile(join(proc, String(pid), 'cmdline'), `${argv.join('\0')}\0`);
  await writeFile(join(proc, String(pid), 'stat'), `${pid} (adb) S ${Array(18).fill('0').join(' ')} ${startTicks} 0 0\n`);
  await symlink('/sdk/platform-tools/adb', join(proc, String(pid), 'exe'));
}

async function capture(env: NodeJS.ProcessEnv, options: Record<string, unknown>) {
  const source = `import { captureAndroidHostSnapshot } from ${JSON.stringify(pathToFileURL(helper).href)};
    await captureAndroidHostSnapshot(${JSON.stringify(options)});`;
  return command(process.execPath, ['-e', source], 30_000, { env });
}

test('records adb identity, server process, devices, host pressure, kernel faults and a redacted server log tail', async () => {
  const { root, proc, output, trace, env } = await fixture();
  try {
    await addServer(proc, 123, ['adb', '-L', 'tcp:5037', 'fork-server', 'server', '--reply-fd', '4'], '4242');
    await mkdir(join(proc, '456'));
    await writeFile(join(proc, '456', 'cmdline'), 'node\0server.js\0fork-server\0');
    const serverLog = join(root, 'adb.log');
    await writeFile(serverLog, `HEAD-MARKER\n${'x'.repeat(300 * 1024)}\n--- adb starting (pid 123) ---\ntoken ${secret}\n`);
    const kernel = join(root, 'kernel.sh');
    await writeFile(kernel, `#!/bin/sh
printf '%s\\n' '2026-11-18T09:01:08 usb 1-1: new device' '2026-11-18T09:01:08 qemu-system-x86[4638]: segfault at 0 ip 1 sp 2 error 4 in libgfxstream_backend.so' '2026-11-18T09:01:09 Out of memory: Killed process 12 (java)'
`, { mode: 0o755 });
    const run = await capture(env, { label: 'after-scenarios', outputDir: output, procRoot: proc, serverLog, kernelCommand: [kernel] });
    assert.equal(run.stdout, '');
    assert.equal(run.stderr, '');
    const snapshot = JSON.parse(await readFile(join(output, 'android-host-after-scenarios.json'), 'utf8'));
    assert.equal(snapshot.label, 'after-scenarios');
    assert.equal(snapshot.adb.pathBinary, await realpath(join(root, 'bin', 'adb')));
    assert.equal(snapshot.adb.sameBinary, true);
    assert.deepEqual(snapshot.adb.version, ['Android Debug Bridge version 1.0.41', 'Version 37.0.1-15733141', 'Installed as /sdk/platform-tools/adb']);
    assert.deepEqual(snapshot.servers, [{ pid: 123, startTicks: '4242', exe: '/sdk/platform-tools/adb', args: '-L tcp:5037 fork-server server --reply-fd 4' }]);
    assert.deepEqual(snapshot.devices.stdout, ['List of devices attached', 'emulator-5554          device product:sdk_gphone64_x86_64 transport_id:3']);
    assert.deepEqual(snapshot.host.memoryKiB, { MemTotal: 16374236, MemAvailable: 9000000, SwapTotal: 4194300, SwapFree: 4194300 });
    assert.equal(snapshot.host.loadavg, '3.91 2.10 1.20 5/900 7000');
    assert.deepEqual(snapshot.host.pressure.cpu, ['some avg10=12.50 avg60=8.00 avg300=2.00 total=100', 'full avg10=0.00 avg60=0.00 avg300=0.00 total=0']);
    assert.deepEqual(snapshot.kernel, [
      '2026-11-18T09:01:08 qemu-system-x86[4638]: segfault at 0 ip 1 sp 2 error 4 in libgfxstream_backend.so',
      '2026-11-18T09:01:09 Out of memory: Killed process 12 (java)',
    ]);
    assert.deepEqual(snapshot.failures, []);
    assert.deepEqual((await readFile(trace, 'utf8')).trim().split('\n'), ['version', 'devices -l']);
    const log = await readFile(join(output, 'android-adb-server-after-scenarios.log'), 'utf8');
    assert.equal(snapshot.serverLog.bytes, (await stat(serverLog)).size);
    assert.equal(snapshot.serverLog.retainedBytes, 256 * 1024);
    assert.ok(log.length <= 256 * 1024);
    assert.match(log, /--- adb starting \(pid 123\) ---\ntoken \[REDACTED\]\n$/u);
    assert.doesNotMatch(log, /HEAD-MARKER/u);
    assert.doesNotMatch(log + JSON.stringify(snapshot), new RegExp(secret, 'u'));
    for (const file of await readdir(output)) assert.equal((await stat(join(output, file))).mode & 0o777, 0o600);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('does not start an adb server and records missing inputs and failed kernel reads', async () => {
  const { root, proc, output, trace, env } = await fixture();
  try {
    const run = await capture(env, { label: 'after-boot', outputDir: output, procRoot: proc, serverLog: join(root, 'missing.log'), kernelCommand: [join(root, 'missing-kernel-reader')] });
    assert.equal(run.stdout, '');
    assert.equal(run.stderr, '');
    const snapshot = JSON.parse(await readFile(join(output, 'android-host-after-boot.json'), 'utf8'));
    assert.deepEqual(snapshot.servers, []);
    assert.equal(snapshot.devices, null);
    assert.equal(snapshot.serverLog, 'missing');
    assert.equal(snapshot.kernel, null);
    assert.equal(snapshot.failures.length, 1);
    assert.match(snapshot.failures[0], /^kernel log: COMMAND_FAILED/u);
    assert.deepEqual((await readFile(trace, 'utf8')).trim().split('\n'), ['version']);
    assert.deepEqual(await readdir(output), ['android-host-after-boot.json']);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('command line rejects invalid labels without output and never fails the step', async () => {
  const { root, output, env } = await fixture();
  try {
    for (const args of [['../escape', output], ['after-boot'], []]) {
      const run = await command(process.execPath, [helper, ...args], 30_000, { env });
      assert.equal(run.stdout, '');
      assert.equal(run.stderr, 'ANDROID_HOST_SNAPSHOT_UNAVAILABLE\n');
    }
    await assert.rejects(readdir(output), { code: 'ENOENT' });
    const run = await command(process.execPath, [helper, 'after-boot', output], 30_000, { env });
    assert.equal(run.stdout, '');
    assert.equal(run.stderr, '');
    assert.ok((await readdir(output)).includes('android-host-after-boot.json'));
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('device action records host state after boot and after scenarios before evidence upload', async () => {
  const runtime = (globalThis as unknown as { Bun: { YAML: { parse(source: string): any } } }).Bun;
  const steps = runtime.YAML.parse(await readFile(repositoryPath('.github/actions/mobile-device-run/action.yml'), 'utf8')).runs.steps;
  const index = (name: string) => steps.findIndex((step: any) => step.name === name);
  const boot = steps[index('Record Android host state after boot')];
  const after = steps[index('Record Android host state after scenarios')];
  assert.equal(boot.if, "inputs.platform == 'android'");
  assert.equal(boot.run, 'set -euo pipefail\nbun tests/mobile/support/android-host-snapshot.ts after-boot "$MOBILE_OUTPUT"\n');
  assert.equal(after.if, "always() && inputs.platform == 'android'");
  assert.equal(after.run, 'set -euo pipefail\nbun tests/mobile/support/android-host-snapshot.ts after-scenarios "$MOBILE_OUTPUT"\n');
  assert.equal(index('Record Android host state after boot'), index('Create and boot Android emulator') + 1);
  assert.ok(index('Record Android host state after boot') < index('Install pinned Android Chrome and Trichrome Library'));
  assert.equal(index('Record Android host state after scenarios'), index('Run installed-PWA upgrade scenarios') + 1);
  assert.ok(index('Record Android host state after scenarios') < index('Upload mobile evidence'));
  assert.ok(index('Upload mobile evidence') < index('Stop owned mobile processes and devices'));
});

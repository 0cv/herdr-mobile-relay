import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer } from 'node:net';
import { AndroidTransportObservation, transportAdbObservation, transportCommand } from '../support/android-transport';

export const androidTransportTests: Array<[string, () => Promise<void>]> = [
  ['Android transport missing owned loopback server stays absent without a daemon', async () => {
    const server = createServer();
    await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
    const address = server.address();
    assert.ok(address && typeof address !== 'string');
    await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    for (const service of ['get-state', 'uptime'] as const) {
      const result = await transportAdbObservation('owned-test-device', service, address.port);
      assert.equal(result.unavailable, true);
      assert.equal(result.timedOut, false);
    }
    await new Promise(resolve => setTimeout(resolve, 300));
    await new Promise<void>((resolve, reject) => {
      server.once('error', reject);
      server.listen(address.port, '127.0.0.1', resolve);
    });
    await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
  }],
  ['Android transport smart socket uses only read services and drains bounded output', async () => {
    const requests: string[] = [];
    const server = createServer(socket => {
      let pending = Buffer.alloc(0);
      socket.on('error', () => undefined);
      socket.on('data', (chunk: Buffer) => {
        pending = Buffer.concat([pending, chunk]);
        if (pending.length < 4) return;
        const length = parseInt(pending.subarray(0, 4).toString(), 16);
        if (pending.length < length + 4) return;
        const request = pending.subarray(4, length + 4).toString();
        pending = pending.subarray(length + 4);
        requests.push(request);
        if (request === 'host-serial:owned-test-device:get-state') socket.end('OKAY0006device');
        else if (request === 'host:transport:owned-test-device') socket.write('OKAY');
        else if (request === 'shell:echo transport-observation; cat /proc/uptime') socket.end(`OKAY${'x'.repeat(1000000)}`);
        else socket.end('FAIL0000');
      });
    });
    await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
    const address = server.address();
    assert.ok(address && typeof address !== 'string');
    try {
      const state = await transportAdbObservation('owned-test-device', 'get-state', address.port);
      assert.equal(state.stdout, 'device');
      assert.equal(state.exitCode, 0);
      const uptime = await transportAdbObservation('owned-test-device', 'uptime', address.port);
      assert.equal(uptime.exitCode, null);
      assert.equal(uptime.unavailable, false);
      assert.equal(uptime.stdout.length, 16384);
      assert.equal(uptime.truncated, true);
      assert.deepEqual(requests, ['host-serial:owned-test-device:get-state', 'host:transport:owned-test-device',
        'shell:echo transport-observation; cat /proc/uptime']);
    } finally {
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    }
  }],
  ['Android transport smart socket timeout and protocol failure are bounded', async () => {
    for (const response of ['', 'FAIL0000', 'OKAYzzzz']) {
      const server = createServer(socket => {
        socket.on('error', () => undefined);
        socket.on('data', () => { if (response) socket.write(response); });
      });
      await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
      const address = server.address();
      assert.ok(address && typeof address !== 'string');
      try {
        const start = Date.now();
        const result = await transportAdbObservation('owned-test-device', 'get-state', address.port, 100);
        assert.equal(result.unavailable, true);
        assert.equal(result.timedOut, response === '');
        assert.ok(Date.now() - start < 2000);
      } finally {
        await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
      }
    }
  }],
  ['Android transport subprocess success and separate streams', async () => {
    const result = await transportCommand(process.execPath, ['-e', 'console.log("ready"); console.error("observed")']);
    assert.equal(result.exitCode, 0);
    assert.equal(result.stdout.trim(), 'ready');
    assert.equal(result.stderr.trim(), 'observed');
    assert.ok(Date.parse(result.endedAt) >= Date.parse(result.startedAt));
    assert.equal(result.unavailable, false);
  }],
  ['Android transport subprocess hung and unavailable are bounded', async () => {
    const start = Date.now();
    const hung = await transportCommand(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], 100);
    assert.equal(hung.timedOut, true);
    assert.ok(Date.now() - start < 2000);
    const absent = await transportCommand('/nonexistent/mobile-transport-command', []);
    assert.equal(absent.unavailable, true);
    assert.equal(absent.exitCode, null);
  }],
  ['Android transport drains over-bound output without exposing credentials or ADB tracing', async () => {
    const result = await transportCommand(process.execPath, ['-e', 'console.log("Authorization: Bearer secret-value"); console.error(process.env.ADB_TRACE || "trace-off"); console.log("x".repeat(1000000))'], 1500, 512);
    assert.equal(result.exitCode, 0);
    assert.equal(result.truncated, true);
    assert.ok(result.stdout.length <= 512);
    assert.ok(!result.stdout.includes('secret-value'));
    assert.equal(result.stderr.trim(), 'trace-off');
  }],
  ['Android transport split flush signal triggers once with safe commands and no driver work', async () => {
    const root = await mkdtemp(join(tmpdir(), 'mobile-transport-'));
    const calls: Array<{ file: string; args: string[] }> = [];
    try {
      const run = async (file: string, args: string[]) => {
        calls.push({ file, args });
        return { startedAt: 'start', endedAt: 'end', stdout: '', stderr: '', exitCode: 0, signal: null, timedOut: false, truncated: false, unavailable: false };
      };
      const observation = new AndroidTransportObservation('emulator-5554', root, run,
        async (serial, service) => run('adb-socket', [serial, service]));
      observation.observe(Buffer.from('123.456 123 456 I unrelated: timeout expired while flushing socket, closing\n'));
      await observation.finish();
      assert.equal(calls.length, 0);
      observation.observe(Buffer.from('123.456 123 456 I adbd    : timeout expired while flushing soc'));
      observation.observe(Buffer.from('ket, closing\n'));
      observation.observe(Buffer.from('124.456 123 456 I adbd: timeout expired while flushing socket, closing\n'));
      await observation.finish();
      assert.equal(calls.length, 5);
      assert.deepEqual(calls.filter(call => call.file === 'adb-socket').map(call => call.args), [
        ['emulator-5554', 'get-state'],
        ['emulator-5554', 'uptime'],
      ]);
      assert.deepEqual(calls[0], { file: 'ps', args: ['-e', '-o', 'pid=,comm=,stat=,wchan=,rss=,pcpu='] });
      assert.ok(!JSON.stringify(calls).includes('argv'));
      const evidence = JSON.parse(await readFile(join(root, 'android-transport-observation.json'), 'utf8'));
      assert.equal(evidence.trigger, 'guest-adbd-flush-timeout');
      assert.equal(evidence.guestEpochSeconds, '123.456');
      assert.equal(evidence.observations.length, 5);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }],
  ['Android transport diagnostic failure cannot replace original error', async () => {
    const original = new Error('original fatal');
    const root = await mkdtemp(join(tmpdir(), 'mobile-transport-failure-'));
    const output = join(root, 'not-a-directory');
    await writeFile(output, '');
    const observation = new AndroidTransportObservation('emulator-5554', output,
      async () => { throw new Error('unavailable'); }, async () => { throw new Error('unavailable'); });
    let caught: unknown;
    try {
      try {
        observation.observe(Buffer.from('123.456 123 456 I adbd: timeout expired while flushing socket, closing\n'));
        throw original;
      } finally {
        await observation.finish();
      }
    } catch (error) {
      caught = error;
    }
    try {
      assert.equal(caught, original);
      assert.equal(observation.failure, 'transport observation could not be saved');
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }],
];

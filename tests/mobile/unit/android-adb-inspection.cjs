'use strict';

const assert = require('node:assert/strict');
const {test} = require('node:test');
const net = require('node:net');
const path = require('node:path');
assert.ok(process.env.APPIUM_HOME, 'Explicit installed fixture required');
const {createAdbInspection} = require(path.join(process.env.APPIUM_HOME, 'node_modules/appium-android-driver/build/lib/commands/context/adb-inspection.cjs'));
const frame = (id, value) => {
  const header = Buffer.alloc(5); header[0] = id; header.writeUInt32LE(value.length, 1);
  return Buffer.concat([header, Buffer.from(value)]);
};
const complete = Buffer.concat([Buffer.from('OKAY'), frame(1, '5301\n'), frame(3, [0])]);

async function fixture(handler, run) {
  const calls = [];
  const sockets = new Set();
  const timers = new Set();
  const later = (fn, ms) => { const timer = setTimeout(() => { timers.delete(timer); fn(); }, ms); timers.add(timer); };
  const server = net.createServer(socket => {
    sockets.add(socket);
    socket.on('error', () => {});
    socket.on('close', () => sockets.delete(socket));
    let pending = Buffer.alloc(0);
    socket.on('data', chunk => {
      pending = Buffer.concat([pending, chunk]);
      if (pending.length < 4) return;
      assert.match(pending.subarray(0, 4).toString(), /^[0-9a-f]{4}$/);
      const length = parseInt(pending.subarray(0, 4).toString(), 16);
      if (pending.length < length + 4) return;
      const service = pending.subarray(4, length + 4).toString();
      pending = pending.subarray(length + 4);
      assert.equal(pending.length, 0);
      calls.push(service);
      if (handler(socket, service, later) === true) return;
      if (service === 'host:version') socket.end('OKAY00040029');
      else if (service === 'host:transport:fixture') socket.write('OKAY');
      else if (service === 'host:list-forward') socket.end('OKAY0000');
      else { assert.equal(service, 'shell,v2,raw:pidof com.android.chrome'); socket.end(complete); }
    });
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  let failed;
  let owned = true;
  const adb = {curDeviceId: 'fixture', executable: {defaultArgs: ['-P', String(server.address().port), '-s', 'fixture']}};
  const client = createAdbInspection(adb, () => { if (!owned) throw new Error('Owner refused'); if (failed) throw failed; }, error => { failed ||= error; return failed; });
  const read = (ms = 500) => client.read(['shell', 'pidof', 'com.android.chrome'], Date.now() + ms);
  const sticky = async () => { const count = calls.length; await assert.rejects(read()); await new Promise(resolve => setTimeout(resolve, 10)); assert.equal(calls.length, count); assert.ok(failed); };
  try { await run({read, client, calls, sticky, adb, refuse: () => { owned = false; }}); }
  finally { for (const timer of timers) clearTimeout(timer); for (const socket of sockets) socket.destroy(); await new Promise(resolve => server.close(resolve)); }
}

test('installed fixed protocol: bounded stdout, version and original serial each read', () => fixture(() => {}, async ({read, client, calls}) => {
  assert.equal(await read(), '5301');
  assert.equal(await client.read(['forward', '--list'], Date.now() + 500), '');
  assert.deepEqual(calls, ['host:version', 'host:transport:fixture', 'shell,v2,raw:pidof com.android.chrome', 'host:version', 'host:list-forward']);
}));

for (const path of ['/proc/5301/status', '/proc/self/status', '/proc/sys/kernel/random/boot_id']) {
  test(`fixed namespace and boot read: ${path}`, () => fixture((socket, service) => {
    if (!service.startsWith('shell,')) return false;
    assert.equal(service, `shell,v2,raw:cat ${path}`);
    socket.end(complete);
    return true;
  }, async ({client, calls}) => {
    assert.equal(await client.read(['shell', 'cat', path], Date.now() + 500), '5301');
    assert.deepEqual(calls, ['host:version', 'host:transport:fixture', `shell,v2,raw:cat ${path}`]);
  }));
}

for (const args of [['shell', 'ls', '-l', '/proc/5301/fd'], ['shell', 'readlink', '/proc/5301/ns/pid'], ['shell', 'cat', '/proc/self/ns/pid'], ['shell', 'cat', '/proc/5301/status;id']]) {
  test(`no privileged or arbitrary namespace read: ${args.join(' ')}`, () => fixture(() => {}, async ({client, calls, sticky}) => {
    await assert.rejects(client.read(args, Date.now() + 500));
    assert.equal(calls.length, 0);
    await sticky();
  }));
}

test('fragmented host headers and shell frames are fully assembled', () => fixture((socket, service, later) => {
  if (service === 'host:transport:fixture') return false;
  const bytes = service === 'host:version' ? Buffer.from('OKAY00040029') : complete;
  [...bytes].forEach((byte, index) => later(() => { if (!socket.destroyed) { socket.write(Buffer.from([byte])); if (index === bytes.length - 1) socket.end(); } }, index));
  return true;
}, async ({read}) => assert.equal(await read(), '5301')));

const shellFailures = {
  'malformed status': Buffer.from('NOPE'),
  'high bit status': Buffer.from([207, 203, 193, 217]),
  'partial status': Buffer.from('OK'),
  'service refusal': Buffer.from('FAIL0006denied'),
  'partial refusal': Buffer.from('FAIL0006den'),
  'malformed refusal length': Buffer.from('FAILzzzz'),
  'missing exit': Buffer.concat([Buffer.from('OKAY'), frame(1, '5301')]),
  'partial frame header': Buffer.from('OKAY\x01\x04'),
  'partial frame body': Buffer.concat([Buffer.from('OKAY'), Buffer.from([1, 4, 0, 0, 0, 1])]),
  'unknown frame': Buffer.concat([Buffer.from('OKAY'), frame(4, [])]),
  'stdin from server': Buffer.concat([Buffer.from('OKAY'), frame(0, [])]),
  'nonzero exit': Buffer.concat([Buffer.from('OKAY'), frame(3, [1])]),
  'wide exit': Buffer.concat([Buffer.from('OKAY'), frame(3, [0, 0])]),
  'empty exit': Buffer.concat([Buffer.from('OKAY'), frame(3, [])]),
  'stderr even with zero exit': Buffer.concat([Buffer.from('OKAY'), frame(2, 'denied'), frame(3, [0])]),
  'invalid utf8': Buffer.concat([Buffer.from('OKAY'), frame(1, [255]), frame(3, [0])]),
  'oversized frame': Buffer.from([79, 75, 65, 89, 1, 1, 0, 16, 0]),
  'stdout aggregate bound': Buffer.concat([Buffer.from('OKAY'), frame(1, 'a'.repeat(600000)), frame(1, 'b'.repeat(600000)), frame(3, [0])]),
  'stderr bound': Buffer.concat([Buffer.from('OKAY'), frame(2, 'a'.repeat(65537)), frame(3, [0])]),
  'frame count bound': Buffer.concat([Buffer.from('OKAY'), ...Array.from({length: 4097}, () => frame(1, [])), frame(3, [0])]),
  'duplicate exit': Buffer.concat([complete, frame(3, [0])]),
  'stdout after exit': Buffer.concat([complete, frame(1, 'late')]),
};
for (const [name, bytes] of Object.entries(shellFailures)) test(`installed shell refusal: ${name}`, () => fixture((socket, service) => {
  if (!service.startsWith('shell,')) return false;
  socket.end(bytes); return true;
}, async ({read, sticky}) => { await assert.rejects(read()); await sticky(); }));

for (const bytes of ['NOPE', 'OKAYzzzz', 'OKAYffffx', 'OKAY00040028', 'OKAY0004002', 'OKAY00040029trailing', 'FAIL0006denied']) test(`installed host refusal: ${bytes}`, () => fixture((socket, service) => {
  assert.equal(service, 'host:version'); socket.end(bytes); return true;
}, async ({read, calls, sticky}) => { await assert.rejects(read()); assert.equal(calls.length, 1); await sticky(); }));

for (const phase of ['host:version', 'host:transport:fixture', 'shell,v2,raw:pidof com.android.chrome']) test(`same deadline covers late ${phase}`, () => fixture((socket, service, later) => {
  if (service !== phase) return false;
  later(() => { if (!socket.destroyed) socket.end(service === 'host:version' ? 'OKAY00040029' : complete); }, 100);
  return true;
}, async ({read, calls, sticky}) => { await assert.rejects(read(30)); const count = calls.length; await new Promise(resolve => setTimeout(resolve, 120)); assert.equal(calls.length, count); await sticky(); }));

for (const bytes of ['OK', 'OKAYextra', 'FAIL0006denied']) test(`transport selection refusal: ${bytes}`, () => fixture((socket, service) => {
  if (service !== 'host:transport:fixture') return false;
  socket.end(bytes); return true;
}, async ({read, calls, sticky}) => { await assert.rejects(read()); assert.deepEqual(calls, ['host:version', 'host:transport:fixture']); await sticky(); }));

test('overlap cancels the pending owned connection with no subsequent sends', () => fixture((socket, service) => service === 'host:version', async ({read, calls, sticky}) => {
  const first = read();
  const rejection = assert.rejects(first);
  await new Promise(resolve => setTimeout(resolve, 10));
  await assert.rejects(read());
  await rejection;
  assert.deepEqual(calls, ['host:version']);
  await sticky();
}));

test('exit without connection completion remains deadline bounded', () => fixture((socket, service) => {
  if (!service.startsWith('shell,')) return false; socket.write(complete); return true;
}, async ({read, sticky}) => { await assert.rejects(read(30)); await sticky(); }));

test('owner refusal between version and transport prevents next send', () => {
  let refuse;
  return fixture((socket, service) => { assert.equal(service, 'host:version'); refuse(); socket.end('OKAY00040029'); return true; }, async (f) => { refuse = f.refuse; await assert.rejects(f.read()); await f.sticky(); });
});

for (const args of [['shell', 'cat', '/proc/1;id/stat'], ['shell', 'su'], ['forward', 'tcp:9'], ['shell', 'cat /proc/1/stat'], ['shell', 'cat', '/proc/0/stat']]) test(`arbitrary request refuses before connect ${JSON.stringify(args)}`, () => fixture(() => {}, async ({client, calls, sticky}) => {
  await assert.rejects(client.read(args, Date.now() + 500)); assert.equal(calls.length, 0); await sticky();
}));

test('absent existing server never starts or retries, even if listener later appears', async () => {
  const server = net.createServer(socket => socket.destroy());
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const port = server.address().port;
  await new Promise(resolve => server.close(resolve));
  let failed;
  const client = createAdbInspection({curDeviceId: 'fixture', executable: {defaultArgs: ['-P', String(port), '-s', 'fixture']}}, () => {}, error => { failed ||= error; return failed; });
  await assert.rejects(client.read(['forward', '--list'], Date.now() + 500));
  let connections = 0;
  server.on('connection', () => connections++);
  await new Promise(resolve => server.listen(port, '127.0.0.1', resolve));
  try { await assert.rejects(client.read(['forward', '--list'], Date.now() + 500)); assert.equal(connections, 0); }
  finally { await new Promise(resolve => server.close(resolve)); }
});

for (const config of [
  {curDeviceId: 'bad serial'}, {adbHost: 'remote'}, {remoteAdbHost: 'localhost'},
  {listenAllNetwork: true}, {remoteAdbPort: 5037}, {adbPort: 1},
  {executable: {defaultArgs: ['-P', '05037', '-s', 'fixture']}},
  {executable: {defaultArgs: ['-P', '65536', '-s', 'fixture']}},
  {executable: {defaultArgs: ['-P', '5037', '-s', 'other']}},
]) test(`unsupported initial configuration refuses ${JSON.stringify(config)}`, () => {
  let failed;
  assert.throws(() => createAdbInspection({curDeviceId: 'fixture', executable: {defaultArgs: ['-P', '5037', '-s', 'fixture']}, ...config}, () => {}, error => { failed = error; return error; }));
  assert.ok(failed);
});

for (const change of [adb => adb.executable.defaultArgs.push('-H', 'remote'), adb => adb.adbHost = 'remote', adb => adb.curDeviceId = 'other', adb => adb.executable.defaultArgs[1] = '1']) test(`latched connection configuration change ${change}`, () => fixture(() => {}, async ({adb, read, calls, sticky}) => {
  change(adb); await assert.rejects(read()); assert.equal(calls.length, 0); await sticky();
}));

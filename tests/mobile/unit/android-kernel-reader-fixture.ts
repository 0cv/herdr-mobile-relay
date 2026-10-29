import assert from 'node:assert/strict';
import { createServer, type Socket } from 'node:net';
import { createAdbInspection } from '../android-appium/adb-inspection.cjs';

export async function kernelReaderFixture(read: (command: string) => Promise<string | Buffer>) {
  const sockets = new Set<Socket>();
  const server = createServer(socket => {
    sockets.add(socket);
    socket.on('close', () => sockets.delete(socket));
    socket.on('error', () => {});
    let pending = Buffer.alloc(0);
    socket.on('data', async chunk => {
      assert.ok(Buffer.isBuffer(chunk));
      pending = Buffer.concat([pending, chunk]);
      if (pending.length < 4) return;
      const length = parseInt(pending.subarray(0, 4).toString(), 16);
      if (pending.length < length + 4) return;
      const service = pending.subarray(4, length + 4).toString();
      pending = pending.subarray(length + 4);
      assert.equal(pending.length, 0);
      if (service === 'host:version') { socket.end('OKAY00040029'); return; }
      if (service === 'host:transport:emulator-5554') { socket.write('OKAY'); return; }
      assert.ok(service.startsWith('shell,v2,raw:'));
      try {
        const bytes = Buffer.from(await read(service.slice(13)));
        const header = Buffer.alloc(5);
        header[0] = 1; header.writeUInt32LE(bytes.length, 1);
        socket.end(Buffer.concat([Buffer.from('OKAY'), header, bytes, Buffer.from([3, 1, 0, 0, 0, 0])]));
      } catch { socket.end('FAIL0007refused'); }
    });
  });
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  assert.ok(address && typeof address !== 'string');
  return {
    create: () => createAdbInspection({curDeviceId: 'emulator-5554', executable: {defaultArgs: ['-P', String(address.port), '-s', 'emulator-5554']}}, () => {}, error => error),
    close: async () => {
      for (const socket of sockets) socket.destroy();
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    },
  };
}

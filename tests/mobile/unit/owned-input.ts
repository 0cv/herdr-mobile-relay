import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { once } from 'node:events';
import { createConnection, createServer, type Socket } from 'node:net';
import { Readable, Writable } from 'node:stream';
import { releaseOwnedInputOnFinish } from '../support/owned-process-runtime.ts';

const sha = (bytes: Buffer) => createHash('sha256').update(bytes).digest('hex');
const gate = () => {
  let resolve!: () => void;
  const promise = new Promise<void>(done => { resolve = done; });
  return { promise, resolve };
};
async function bounded<T>(promise: Promise<T>, label: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([promise, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error(`${label}: natural peer EOF not observed`)), 2_000);
    })]);
  } finally { clearTimeout(timer); }
}

// Like the affected endpoint, finish alone does not release this owned socket.
class OwnedSocketWriter extends Writable {
  destroys = 0;
  readonly writing = gate();
  readonly finalizing = gate();
  writeGate = gate();
  finalGate = gate();
  writeFailure?: Error;
  finalFailure?: Error;
  readonly socket: Socket;
  constructor(socket: Socket) {
    super({ autoDestroy: false, highWaterMark: 1024 });
    this.socket = socket;
  }
  override _write(chunk: Buffer, _encoding: BufferEncoding, callback: (error?: Error | null) => void): void {
    this.writing.resolve();
    void this.writeGate.promise.then(() => {
      if (this.writeFailure) callback(this.writeFailure);
      else this.socket.write(chunk, callback);
    });
  }
  override _final(callback: (error?: Error | null) => void): void {
    this.finalizing.resolve();
    void this.finalGate.promise.then(() => callback(this.finalFailure));
  }
  override _destroy(error: Error | null, callback: (error?: Error | null) => void): void {
    this.destroys++;
    this.socket.destroy();
    callback(error);
  }
}

async function socketFixture(expectedBytes = 0) {
  const server = createServer({ allowHalfOpen: true });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const address = server.address();
  assert.ok(address && typeof address !== 'string');
  const accepted = once(server, 'connection');
  const socket = createConnection({ port: address.port, host: '127.0.0.1', allowHalfOpen: true });
  await once(socket, 'connect');
  const [peer] = await accepted as [Socket];
  const chunks: Buffer[] = [];
  const eof = gate();
  const received = gate();
  let bytes = 0;
  peer.on('data', chunk => {
    chunks.push(Buffer.from(chunk));
    bytes += chunk.length;
    if (bytes >= expectedBytes) received.resolve();
  });
  peer.once('end', eof.resolve);
  const writer = new OwnedSocketWriter(socket);
  const errors: Error[] = [];
  writer.on('error', error => errors.push(error));
  // Observe peer-initiated closure without treating it as a successful finish.
  socket.on('error', error => writer.destroy(error));
  socket.once('end', () => { if (!writer.writableFinished) writer.destroy(new Error('premature peer close')); });
  socket.resume();
  let ended = false;
  peer.once('end', () => { ended = true; });
  return {
    writer, peer, errors, eof: eof.promise, received: received.promise,
    bytes: () => Buffer.concat(chunks), ended: () => ended,
    async barrier() {
      const received = once(socket, 'data');
      peer.write('barrier');
      await bounded(received, 'reverse socket barrier');
    },
    async cleanup() {
      writer.writeGate.resolve();
      writer.finalGate.resolve();
      writer.destroy();
      socket.destroy();
      peer.destroy();
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    },
  };
}

export const ownedInputTests: Array<[string, () => Promise<void>]> = [];
for (const size of [9467, 256 * 1024 + 17]) {
  ownedInputTests.push([`owned input finish releases socket with intact ${size} bytes and natural EOF`, async () => {
    const fixture = await socketFixture(size);
    const { writer } = fixture;
    const payload = Buffer.alloc(size);
    for (let index = 0; index < size; index++) payload[index] = 33 + index % 90;
    try {
      releaseOwnedInputOnFinish(writer);
      Readable.from([payload]).pipe(writer);
      await bounded(writer.writing.promise, 'write entry');
      await fixture.barrier();
      assert.equal(writer.writableLength, size);
      assert.equal(writer.destroys, 0, 'pending write must not release endpoint');
      assert.equal(fixture.ended(), false);
      writer.writeGate.resolve();
      await bounded(writer.finalizing.promise, 'final entry');
      await fixture.barrier();
      assert.equal(writer.writableLength, 0);
      assert.equal(writer.writableEnded, true);
      assert.equal(writer.writableFinished, false);
      assert.equal(writer.destroys, 0, 'empty queue/source end must not bypass pending final callback');
      assert.equal(fixture.ended(), false);
      await bounded(fixture.received, 'peer byte receipt');
      assert.equal(fixture.bytes().length, size);
      assert.equal(sha(fixture.bytes()), sha(payload));
      const closed = once(writer, 'close');
      writer.finalGate.resolve();
      await bounded(fixture.eof, 'positive EOF rule');
      await bounded(closed, 'writer close');
      assert.equal(writer.writableFinished, true);
      assert.equal(writer.destroys, 1);
      assert.equal(fixture.bytes().length, size);
      assert.equal(sha(fixture.bytes()), sha(payload));
      assert.deepEqual(fixture.errors, []);
      for (const event of ['finish', 'close']) assert.equal(writer.listenerCount(event), 0);
      assert.equal(writer.listenerCount('error'), 1, 'only the test failure observer remains');
      releaseOwnedInputOnFinish(writer);
      assert.equal(writer.destroys, 1);
    } finally { await fixture.cleanup(); }
  }]);
}

ownedInputTests.push(['owned input finish handles already finished and already destroyed endpoints', async () => {
  const fixture = await socketFixture();
  try {
    const { writer } = fixture;
    writer.writeGate.resolve();
    writer.finalGate.resolve();
    const finished = once(writer, 'finish');
    writer.end();
    await finished;
    assert.equal(writer.destroys, 0);
    await fixture.barrier();
    assert.equal(fixture.ended(), false);
    releaseOwnedInputOnFinish(writer);
    await bounded(fixture.eof, 'already finished EOF');
    releaseOwnedInputOnFinish(writer);
    assert.equal(writer.destroys, 1);
  } finally { await fixture.cleanup(); }
  const destroyed = new Writable({ autoDestroy: false, write(_chunk, _encoding, callback) { callback(); } });
  const closed = once(destroyed, 'close');
  destroyed.destroy();
  releaseOwnedInputOnFinish(destroyed);
  await closed;
  releaseOwnedInputOnFinish(destroyed);
  for (const event of ['finish', 'close', 'error']) assert.equal(destroyed.listenerCount(event), 0);
}]);

for (const failure of ['write', 'final', 'peer-close'] as const) {
  ownedInputTests.push([`owned input finish preserves ${failure} failure without successful release`, async () => {
    const fixture = await socketFixture();
    const { writer } = fixture;
    try {
      releaseOwnedInputOnFinish(writer);
      const expected = new Error(`${failure} failed`);
      if (failure === 'write') writer.writeFailure = expected;
      if (failure === 'final') writer.finalFailure = expected;
      const error = new Promise<Error>(resolve => writer.once('error', resolve));
      writer.end(Buffer.alloc(9467, 42));
      await writer.writing.promise;
      if (failure === 'peer-close') {
        fixture.peer.end();
      } else {
        writer.writeGate.resolve();
        writer.finalGate.resolve();
      }
      const observed = await bounded(error, 'expected failure');
      assert.match(observed.message, failure === 'peer-close' ? /premature peer close/u : /failed/u);
      assert.equal(writer.writableFinished, false);
      assert.equal(writer.destroys, failure === 'peer-close' ? 1 : 0);
      assert.equal(writer.listenerCount('finish'), 0);
      assert.equal(writer.listenerCount('close'), 0);
      assert.equal(writer.listenerCount('error'), 1);
      releaseOwnedInputOnFinish(writer);
      assert.equal(writer.listenerCount('finish'), 0);
    } finally { await fixture.cleanup(); }
  }]);
}

import assert from 'node:assert/strict';
import { createConnection } from 'node:net';
import { mkdir, lstat } from 'node:fs/promises';
import { join } from 'node:path';
import { createActivitySource, startActivityServer, ACTIVITY_LIMITS } from '../../relay/pi-command-bridge/activity.mjs';

const work = process.env.PI_SOCKET_TEST_WORK;
assert(work && process.env.HOME === join(work, 'home') && process.env.PI_OFFLINE === '1');
assert.equal(ACTIVITY_LIMITS.streams, 8); assert.equal(ACTIVITY_LIMITS.pages, 8); assert.equal(ACTIVITY_LIMITS.pending, 16); assert.equal(ACTIVITY_LIMITS.handshakeMs, 1000);
const directory = join(work, 'sockets');
await mkdir(directory, { recursive: true, mode: 0o700 });
const identity = { instance: 'a'.repeat(64), pane: 'private-capacity-fixture', session: 'fixture', pid: process.pid };
const entries = new Map([
  ['user', { id: 'user', parentId: null, type: 'message', message: { role: 'user', content: 'prompt' } }],
  ['older', { id: 'older', parentId: 'user', type: 'message', message: { role: 'assistant', content: [{ type: 'text', text: 'older answer' }] } }],
  ['newest', { id: 'newest', parentId: 'older', type: 'message', message: { role: 'assistant', content: [{ type: 'text', text: 'newest answer' }] } }],
]);
const source = createActivitySource({ getLeafId: () => 'newest', getEntry: id => entries.get(id), getSessionId: () => identity.session });
const close = await startActivityServer({ directory, identity, source });
const path = join(directory, `${process.pid}.activity.sock`);
const clients = [];
const wait = async (condition, label) => {
  const deadline = Date.now() + 2000;
  while (!condition()) {
    assert(Date.now() < deadline, label);
    await new Promise(resolve => setTimeout(resolve, 5));
  }
};
function connect() {
  const socket = createConnection({ path, allowHalfOpen: true });
  const client = { socket, frames: [], ended: false, peerEnded: false, connected: false, error: undefined };
  clients.push(client);
  client.closed = new Promise(resolve => socket.once('close', () => { client.ended = true; resolve(); }));
  socket.once('connect', () => { client.connected = true; });
  socket.once('end', () => { client.peerEnded = true; });
  socket.on('error', error => { client.error = error.code; });
  let pending = '';
  socket.on('data', chunk => {
    pending += chunk;
    let index;
    while ((index = pending.indexOf('\n')) >= 0) {
      try { client.frames.push(JSON.parse(pending.slice(0, index))); }
      catch { client.error = 'invalid_json'; socket.destroy(); return; }
      pending = pending.slice(index + 1);
    }
  });
  return client;
}
async function connected(count) {
  const batch = Array.from({ length: count }, connect);
  await wait(() => batch.every(client => client.connected || client.ended), 'connections did not settle');
  assert(batch.every(client => client.connected && !client.ended), 'supported connections were rejected before classification');
  return batch;
}
function request(client, operation, cursor = '') {
  client.socket.write(JSON.stringify({ ...identity, operation, cursor, limit: 1, challenge: 'b'.repeat(32) }) + '\n');
}
async function frames(batch) {
  await wait(() => batch.every(client => client.frames.length || client.ended || client.peerEnded), 'responses did not settle');
  return batch.map(client => {
    assert(!client.error && client.frames.length, `request rejected: ${client.error || 'closed without a frame'}`);
    assert.equal(client.frames[0].challenge, 'b'.repeat(32));
    return client.frames[0];
  });
}
async function dispose(batch) {
  for (const client of batch) client.socket.destroy();
  await Promise.all(batch.map(client => client.closed));
  await new Promise(resolve => setImmediate(() => setImmediate(resolve)));
}
async function pages(cursors) {
  const batch = await connected(8);
  batch.forEach((client, index) => request(client, 'page', cursors?.[index] || ''));
  return { clients: batch, frames: await frames(batch) };
}
try {
  const subscriptions = await connected(1);
  request(subscriptions[0], 'subscribe');
  assert.equal((await frames(subscriptions))[0].kind, 'snapshot');
  const newest = await pages();
  assert(newest.frames.every(frame => frame.kind === 'page' && frame.page.entries[0].id === 'pi:newest' && frame.page.has_more));
  const cursors = newest.frames.map(frame => frame.page.next_cursor);
  await dispose(newest.clients);
  const older = await pages(cursors);
  assert(older.frames.every(frame => frame.page.entries[0].id === 'pi:older'));
  await dispose(older.clients);
  const more = await connected(7);
  more.forEach(client => request(client, 'subscribe'));
  assert((await frames(more)).every(frame => frame.kind === 'snapshot'));
  subscriptions.push(...more);
  const excessStream = (await connected(1))[0]; request(excessStream, 'subscribe');
  await wait(() => excessStream.ended || excessStream.peerEnded, 'excess stream was not rejected');
  assert.equal(excessStream.frames.length, 0);
  const fullPages = await pages();
  const excessPage = (await connected(1))[0]; request(excessPage, 'page');
  await wait(() => excessPage.ended || excessPage.peerEnded || excessPage.frames.length, 'excess page was not refused');
  assert.equal(excessPage.frames.length, 0);
  await dispose(fullPages.clients);
  const pending = await connected(16);
  const excessPending = connect();
  await wait(() => excessPending.ended || excessPending.peerEnded, 'pending handshake limit was not enforced');
  assert.equal(excessPending.frames.length, 0);
  await wait(() => pending.every(client => client.ended || client.peerEnded), 'quiet handshakes did not expire');
  await dispose(pending);
  const tricklers = await connected(16);
  let trickleRounds = 0;
  const trickle = () => {
    trickleRounds++;
    for (const client of tricklers) if (!client.ended && !client.peerEnded) client.socket.write(' ');
  };
  trickle();
  const dripping = setInterval(trickle, 100);
  try {
    await wait(() => tricklers.every(client => client.ended || client.peerEnded), 'trickled handshakes exceeded the absolute deadline');
  } finally { clearInterval(dripping); }
  assert(trickleRounds >= 4, 'trickle regression did not keep clients active');
  await dispose(tricklers);
  const recovered = await pages();
  assert(recovered.frames.every(frame => frame.page.entries[0].id === 'pi:newest'));
  await dispose(recovered.clients);
  await dispose([subscriptions.pop()]);
  const replacement = (await connected(1))[0]; request(replacement, 'subscribe');
  assert.equal((await frames([replacement]))[0].kind, 'snapshot');
  subscriptions.push(replacement);
  source.event('message_start', { message: { role: 'assistant', content: [] } });
  source.event('message_update', { message: { role: 'assistant', content: [{ type: 'text', text: 'after concurrent paging' }] } });
  await wait(() => subscriptions.every(client => client.frames.some(frame => frame.kind === 'batch' && frame.entries.some(entry => entry.text === 'after concurrent paging'))), 'paging interrupted an active subscription');
  await dispose(subscriptions);
  const simultaneous = await connected(16);
  simultaneous.forEach((client, index) => request(client, index < 8 ? 'subscribe' : 'page'));
  const simultaneousFrames = await frames(simultaneous);
  assert.equal(simultaneousFrames.filter(frame => frame.kind === 'snapshot').length, 8);
  assert.equal(simultaneousFrames.filter(frame => frame.kind === 'page').length, 8);
  await dispose(simultaneous);
  console.log(JSON.stringify({ runtime: process.versions.node, concurrentPagesWithOneStream: 8, concurrentOlderPages: 8, saturatedStreams: 8, concurrentPagesWithFullStreams: 8, simultaneousStreamAndPageHandshakes: 16, independentLimitsEnforced: true, pendingLimit: 16, quietHandshakeExpiry: 16, tricklingHandshakeExpiry: 16, trickleRounds, subscriptionContinuity: true, capacityReleased: true }));
} finally {
  await dispose(clients);
  await close();
  await assert.rejects(lstat(path), { code: 'ENOENT' });
}

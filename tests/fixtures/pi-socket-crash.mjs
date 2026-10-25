import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createConnection } from 'node:net';
import { mkdir, lstat, chmod, writeFile, readFile, unlink, symlink } from 'node:fs/promises';
import { join } from 'node:path';
import { startBridge } from '../../relay/pi-command-bridge/bridge.mjs';
import { createActivitySource, startActivityServer } from '../../relay/pi-command-bridge/activity.mjs';

const work = process.env.PI_SOCKET_TEST_WORK;
assert(work && process.env.HOME === join(work, 'home') && process.env.PI_OFFLINE === '1');
const directory = join(work, 'sockets');
await mkdir(directory, { recursive: true, mode: 0o700 });
const identity = { instance: 'a'.repeat(64), pane: 'private-fixture', session: 'fixture', pid: process.pid };
const manager = { getLeafId: () => null, getEntry: () => undefined, getSessionId: () => identity.session };
let recovered = 0;
for (const kind of ['catalog', 'activity']) {
  const path = join(directory, `${process.pid}${kind === 'activity' ? '.activity' : ''}.sock`);
  const start = async () => {
    if (kind === 'catalog') return startBridge({ ...identity, directory, catalog: () => ({ commands: [] }) });
    const source = createActivitySource(manager);
    try { return await startActivityServer({ directory, identity, source }); } catch (error) { source.close(); throw error; }
  };
  const child = spawn(process.execPath, ['--input-type=module', '-e', 'import net from "node:net";import{chmod}from"node:fs/promises";const server=net.createServer();server.listen(process.argv[1],async()=>{await chmod(process.argv[1],0o600);process.stdout.write("ready\\n")});', path], { env: { PATH: process.env.PATH, HOME: process.env.HOME }, stdio: ['ignore', 'pipe', 'pipe'] });
  let errors = '';
  child.stderr.on('data', data => { errors += data.toString(); });
  const ended = new Promise(resolve => child.once('exit', (code, signal) => resolve({ code, signal })));
  let readyTimer;
  try {
    await new Promise((resolve, reject) => {
      child.once('error', reject);
      child.once('exit', () => reject(new Error(`Private listener exited before readiness: ${errors}`)));
      child.stdout.once('data', resolve);
      readyTimer = setTimeout(() => reject(new Error('Private listener readiness timed out')), 2000);
    });
  } catch (error) { child.kill('SIGKILL'); throw error; }
  finally { clearTimeout(readyTimer); }
  child.kill('SIGKILL');
  assert.equal((await ended).signal, 'SIGKILL', errors);
  assert((await lstat(path)).isSocket());
  const close = await start();
  try {
  const frame = await new Promise((resolve, reject) => {
    const socket = createConnection(path); let pending = ''; let received = false;
    socket.setTimeout(1000, () => socket.destroy(new Error('Recovered socket did not respond')));
    socket.once('error', reject);
    socket.once('close', () => { if (!received) reject(new Error('Recovered socket closed before responding')); });
    socket.once('connect', () => socket.write(JSON.stringify({ ...identity, challenge: 'b'.repeat(32), operation: kind === 'catalog' ? 'catalog' : 'page' }) + '\n'));
    socket.on('data', chunk => {
      pending += chunk;
      if (!pending.includes('\n')) return;
      received = true; socket.destroy();
      try { resolve(JSON.parse(pending.slice(0, pending.indexOf('\n')))); } catch (error) { reject(error); }
    });
  });
  assert.equal(frame.pid, process.pid);
  assert.equal(frame.challenge, 'b'.repeat(32));
  const inode = (await lstat(path)).ino;
  await assert.rejects(start());
  assert.equal((await lstat(path)).ino, inode);
  } finally { await close(); }
  await assert.rejects(lstat(path), { code: 'ENOENT' });
  recovered++;
  await writeFile(path, 'preserve unrelated file', { mode: 0o600 });
  await assert.rejects(start());
  assert.equal(await readFile(path, 'utf8'), 'preserve unrelated file');
  await unlink(path);
  const target = join(directory, `${kind}.unrelated`);
  await writeFile(target, 'preserve symlink target', { mode: 0o600 });
  await symlink(target, path);
  await assert.rejects(start());
  assert((await lstat(path)).isSymbolicLink());
  assert.equal(await readFile(target, 'utf8'), 'preserve symlink target');
  await unlink(path); await unlink(target);
  await chmod(directory, 0o755);
  await assert.rejects(start());
  await chmod(directory, 0o700);
}
console.log(JSON.stringify({ runtime: process.versions.node, killedPrivateListeners: 2, recoveredReusedPidPaths: recovered, liveOwnersAndUnrelatedFilesPreserved: true }));

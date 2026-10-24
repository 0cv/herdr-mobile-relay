import assert from 'node:assert/strict';
import { mkdir, lstat } from 'node:fs/promises';
import { join } from 'node:path';
import { startBridge } from '../../relay/pi-command-bridge/bridge.mjs';
import { createActivitySource, startActivityServer } from '../../relay/pi-command-bridge/activity.mjs';
const work = process.env.PI_SOCKET_TEST_WORK;
assert(work && process.env.HOME === join(work, 'home') && process.env.PI_OFFLINE === '1');
const directory = join(work, 'sockets');
await mkdir(directory, { recursive: true, mode: 0o700 });
const identity = { instance: 'a'.repeat(64), pane: 'private-fixture', session: 'fixture', pid: process.pid };
const manager = { getLeafId: () => null, getEntry: () => undefined, getSessionId: () => identity.session };
for (const kind of ['catalog', 'activity']) {
  const path = join(directory, `${process.pid}${kind === 'activity' ? '.activity' : ''}.sock`);
  for (let attempt = 0; attempt < 3; attempt++) {
    const close = kind === 'catalog' ? await startBridge({ ...identity, directory, catalog: () => ({ commands: [] }) }) : await startActivityServer({ directory, identity, source: createActivitySource(manager) });
    assert((await lstat(path)).isSocket());
    await close(); await close();
    await assert.rejects(lstat(path), { code: 'ENOENT' });
  }
}
console.log(JSON.stringify({ runtime: process.versions.node, samePidRestarts: 6, pathsAbsentBeforeDirectoryRemoval: true }));

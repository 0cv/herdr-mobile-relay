'use strict';

const {test} = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const {createRequire} = require('node:module');
const path = require('node:path');
assert.ok(process.env.APPIUM_HOME, 'Owned installed fixture required');
const installedRequire = createRequire(path.join(process.env.APPIUM_HOME, 'node_modules/appium-android-driver/build/lib/commands/context/target-inspection.cjs'));
const {WebSocketServer} = installedRequire('ws');
const {inspectTargets, associateBrowserProcess} = installedRequire('./target-inspection.cjs');

async function fixture(mode, run) {
  const calls = [];
  const peers = new Set();
  const timers = new Set();
  let connections = 0;
  let pidReads = 0;
  const browserPath = mode === 'bare' ? '/devtools/browser' : '/devtools/browser/owned';
  const server = http.createServer((req, res) => {
    if (mode === 'http-stall') return;
    if (mode === 'http-partial') { res.writeHead(200, {'Content-Length': 900}); res.write('{'); res.destroy(); return; }
    if (mode === 'http-large') { res.end('x'.repeat(70000)); return; }
    if (mode === 'http-malformed') { res.end('{'); return; }
    res.end(JSON.stringify({Browser: 'Chrome/131', webSocketDebuggerUrl: mode === 'foreign-endpoint' ? 'ws://127.0.0.1:1/devtools/browser/foreign' : `ws://127.0.0.1:${server.address().port}${browserPath}`}));
  });
  server.on('connection', (peer) => { peers.add(peer); peer.on('close', () => peers.delete(peer)); });
  const wss = new WebSocketServer({noServer: true});
  server.on('upgrade', (req, socket, head) => {
    if (mode === 'open-stall') return;
    wss.handleUpgrade(req, socket, head, (ws) => wss.emit('connection', ws));
  });
  let inventoryCount = 0;
  let evaluations = 0;
  const target = (id) => ({targetId: id, type: 'page', url: 'https://app/', title: 'same', attached: true});
  wss.on('connection', (ws) => {
    connections++;
    ws.on('message', (raw) => {
    const message = JSON.parse(raw);
    calls.push(message);
    if (mode === 'read-stall') return;
    if (mode === 'disconnect') { ws.terminate(); return; }
    if (mode === 'frame-partial') { ws._socket.write(Buffer.from([0x81, 126, 0, 100, 123])); return; }
    if (mode === 'frame-large') { ws.send('x'.repeat(70000)); return; }
    if (mode === 'frame-malformed') { ws.send('{'); return; }
    if (mode === 'event') { ws.send(JSON.stringify({method: 'Target.targetCreated', params: {}})); return; }
    let result;
    switch (message.method) {
      case 'SystemInfo.getProcessInfo': {
        assert.equal(message.sessionId, undefined);
        assert.deepEqual(message.params, {});
        pidReads++;
        let entries = [{type: 'browser', id: 123, cpuTime: 0}];
        const invalidIds = {'pid-string': '123', 'pid-fraction': 123.5, 'pid-zero': 0, 'pid-negative': -1, 'pid-overflow': 2147483648, 'pid-unsafe': 9007199254740992, 'pid-missing-id': undefined, 'pid-wrong': 124};
        if (Object.hasOwn(invalidIds, mode)) entries[0].id = invalidIds[mode];
        if (mode === 'pid-after-wrong' && pidReads === 2) entries[0].id = 124;
        if (mode === 'pid-missing') entries = [];
        if (mode === 'pid-absent') entries = undefined;
        if (mode === 'pid-duplicate') entries.push({type: 'browser', id: 124, cpuTime: 0});
        if (mode === 'pid-duplicate-id') entries.push({type: 'renderer', id: 123, cpuTime: 0});
        if (mode === 'pid-children') entries.push({type: 'renderer', id: 124, cpuTime: 0}, {type: 'GPU', id: 125, cpuTime: 1.5});
        if (mode === 'pid-child-invalid') entries.push({type: 'renderer', id: '124', cpuTime: 0});
        if (mode === 'pid-many') entries = Array.from({length: 257}, (_, i) => ({type: i ? 'renderer' : 'browser', id: 123 + i, cpuTime: 0}));
        if (mode === 'pid-type') entries[0].type = 5;
        if (mode === 'pid-no-browser') entries[0].type = 'renderer';
        if (mode === 'pid-no-cpu') delete entries[0].cpuTime;
        if (mode === 'pid-cpu-string') entries[0].cpuTime = '0';
        if (mode === 'pid-cpu-negative') entries[0].cpuTime = -1;
        if (mode === 'pid-null') entries[0] = null;
        result = {processInfo: entries};
        break;
      }
      case 'Target.getTargets': {
        inventoryCount++;
        let infos = [target('bootstrap'), target('installed')];
        if (mode === 'duplicate') infos.push(target('installed'));
        if (mode === 'invalid') delete infos[0].type;
        if (mode === 'unmapped') infos.push(target('extra'));
        if (mode === 'changed' && inventoryCount > 1) infos[0].title = 'changed';
        if (mode === 'disappeared' && inventoryCount > 1) infos.pop();
        if (mode === 'replaced' && inventoryCount > 1) infos[1].targetId = 'replacement';
        if (mode === 'unknown') infos.push({...target('worker'), type: 'service_worker'});
        result = {targetInfos: infos};
        break;
      }
      case 'Target.attachToTarget':
        result = {sessionId: message.params.targetId};
        ws.send(JSON.stringify({method: 'Target.attachedToTarget', params: {sessionId: mode === 'wrong-attach-session' ? 'wrong' : result.sessionId, targetInfo: target(mode === 'wrong-target' ? 'wrong' : message.params.targetId), waitingForDebugger: false}}));
        break;
      case 'DOM.getDocument':
        result = {root: {nodeType: 9, backendNodeId: mode === 'document-replaced' && evaluations > 0 ? 999 : message.sessionId === 'installed' ? 2 : 1, documentURL: 'https://app/'}};
        break;
      case 'Runtime.evaluate': {
        evaluations++;
        const standalone = mode === 'ambiguous' || message.sessionId === 'installed';
        result = {result: {type: 'string', value: JSON.stringify({href: 'https://app/', origin: 'https://app', standalone, provider: standalone ? 'android-standalone' : 'browser', timeOrigin: 100})}};
        break;
      }
      default: throw new Error(`Unexpected command ${message.method}`);
    }
    const response = {id: mode === 'wrong-request' ? message.id + 1 : message.id, result, ...(message.sessionId ? {sessionId: mode === 'wrong-session' ? 'foreign' : message.sessionId} : {})};
    if (mode === 'pid-child-route') response.sessionId = 'installed';
    if (mode === 'child-root-route' && message.sessionId) delete response.sessionId;
    if (mode === 'pid-error') response.error = {code: -32601, message: 'Unsupported'};
    if (mode === 'pid-stale' && pidReads === 2) response.id = 1;
    if (mode === 'late' || (mode === 'pid-after-late' && pidReads === 2)) {
      timers.add(setTimeout(() => { if (ws.readyState === 1) ws.send(JSON.stringify(response)); }, 150));
      return;
    }
    if (mode === 'pid-repeat') {
      ws._socket.cork();
      try { ws.send(JSON.stringify(response)); ws.send(JSON.stringify(response)); }
      finally { ws._socket.uncork(); }
    } else ws.send(JSON.stringify(response));
    if (mode === 'final-disconnect' && pidReads === 2) ws.close();
    });
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const owner = {};
  let failures = 0;
  let snapshots = 0;
  const snapshot = {endpoint: {host: '127.0.0.1', port: server.address().port, browserPath}, browserVersion: 'Chrome/131', handles: ['bootstrap', 'installed'], selectedHandle: 'installed'};
  const contract = {
    expectedBrowserPid: 123,
    deadline: Date.now() + (['late', 'pid-after-late', 'http-stall', 'open-stall', 'read-stall', 'frame-partial'].includes(mode) ? 100 : 2000),
    assertOwner: () => { if (mode === 'owner-before' || (mode === 'owner-during' && calls.length >= 3)) throw new Error('Owner changed'); },
    failure: () => { failures++; },
    snapshot: async () => {
      snapshots++;
      if (mode === 'final-disconnect' && snapshots > 1) await new Promise((resolve) => setTimeout(resolve, 30));
      return structuredClone(snapshot);
    },
    validateSnapshot: async (_owner, before, after) => { assert.deepEqual(before, after); if (mode === 'owner-after' && snapshots > 1) throw new Error('Native owner lost'); },
    validateSelectedDocument: async (_owner, result) => {
      assert.equal(result.selectedHandle, 'installed');
      assert.equal(result.observations.find((o) => o.targetId === 'installed').document.backendNodeId, 2);
      if (mode === 'selected-refused') throw new Error('Selected association refused');
      if (mode === 'final-selected-disconnect') {
        for (const client of wss.clients) client.close();
        await new Promise((resolve) => setTimeout(resolve, 30));
      }
    },
  };
  if (mode === 'expired') contract.deadline = Date.now() - 1;
  try { await run({owner, contract, calls, connections: () => connections, failures: () => failures}); }
  finally {
    for (const timer of timers) clearTimeout(timer);
    for (const client of wss.clients) client.terminate();
    for (const peer of peers) peer.destroy();
    await new Promise((resolve) => server.close(resolve));
    wss.close();
  }
}

for (const mode of ['healthy', 'ambiguous', 'unknown', 'pid-children', 'bare']) {
  test(mode, async () => fixture(mode, async ({owner, contract, calls, connections, failures}) => {
    const result = await inspectTargets(owner, contract);
    assertAssociation(result, calls, contract);
    assert.equal(connections(), 1);
    assert.equal(result.kind, 'bounded-nonactivating-observation');
    assert.equal(result.observations.length, 2);
    assert.equal(result.observations.filter((o) => o.document.standalone).length, mode === 'ambiguous' ? 2 : 1);
    assert.equal(result.targets.length, mode === 'unknown' ? 3 : 2);
    assert.equal(failures(), 0);
    assert(calls.every((c) => ['SystemInfo.getProcessInfo', 'Target.getTargets', 'Target.attachToTarget', 'DOM.getDocument', 'Runtime.evaluate'].includes(c.method)));
    const expressions = calls.filter((c) => c.method === 'Runtime.evaluate').map((c) => c.params.expression);
    assert.equal(new Set(expressions).size, 1);
  }));
}
for (const mode of ['pid-string', 'pid-fraction', 'pid-zero', 'pid-negative', 'pid-overflow', 'pid-unsafe', 'pid-missing-id', 'pid-wrong', 'pid-after-wrong', 'pid-missing', 'pid-absent', 'pid-duplicate', 'pid-duplicate-id', 'pid-child-invalid', 'pid-many', 'pid-type', 'pid-no-browser', 'pid-no-cpu', 'pid-cpu-string', 'pid-cpu-negative', 'pid-null', 'pid-child-route', 'child-root-route', 'pid-error', 'pid-stale', 'pid-repeat', 'pid-after-late', 'final-disconnect', 'final-selected-disconnect', 'duplicate', 'invalid', 'unmapped', 'changed', 'disappeared', 'replaced', 'wrong-target', 'wrong-attach-session', 'wrong-request', 'wrong-session', 'event', 'http-large', 'http-partial', 'http-malformed', 'frame-large', 'frame-partial', 'frame-malformed', 'http-stall', 'open-stall', 'read-stall', 'expired', 'owner-before', 'owner-during', 'owner-after', 'selected-refused', 'disconnect', 'late', 'foreign-endpoint', 'document-replaced']) {
  test(mode, async () => fixture(mode, async ({owner, contract, calls, failures}) => {
    await assert.rejects(inspectTargets(owner, contract));
    assert.equal(failures(), 1);
    const count = calls.length;
    await assert.rejects(inspectTargets(owner, {...contract, deadline: Date.now() + 1000}));
    await assert.rejects(associateBrowserProcess(owner, {...contract, deadline: Date.now() + 1000}));
    assert.equal(failures(), 1);
    await new Promise((resolve) => setTimeout(resolve, mode.includes('late') ? 170 : 5));
    assert.equal(calls.length, count);
    if (['expired', 'owner-before', 'foreign-endpoint'].includes(mode)) assert.equal(count, 0);
  }));
}

test('original PID cannot rebase between entry points', async () => fixture('healthy', async ({owner, contract, calls, connections}) => {
  await associateBrowserProcess(owner, contract);
  await inspectTargets(owner, contract);
  assert.equal(connections(), 2);
  const count = calls.length;
  await assert.rejects(associateBrowserProcess(owner, {...contract, expectedBrowserPid: 124}), /Original browser process changed/);
  await assert.rejects(inspectTargets(owner, contract), /Original browser process changed/);
  assert.equal(calls.length, count);
  assert.equal(connections(), 2);
}));

function assertAssociation(result, calls, contract) {
  const association = result.processAssociation;
  assert.equal(association.kind, 'bounded-sequential-service-to-process');
  assert.equal(calls[0].method, 'SystemInfo.getProcessInfo');
  assert.equal(calls.at(-1).method, 'SystemInfo.getProcessInfo');
  assert.equal(calls.filter((c) => c.method === 'SystemInfo.getProcessInfo').length, 2);
  assert.equal(association.before.connectionId, association.after.connectionId);
  assert.match(association.before.connectionId, /^[a-f0-9-]{36}$/);
  for (const [observation, call] of [[association.before, calls[0]], [association.after, calls.at(-1)]]) {
    assert.deepEqual(Object.keys(observation).sort(), ['completedAt', 'connectionId', 'pid', 'requestId', 'startedAt']);
    assert.equal(observation.pid, 123);
    assert.equal(observation.requestId, call.id);
    assert.equal(call.sessionId, undefined);
    assert.deepEqual(call.params, {});
    assert(observation.startedAt <= observation.completedAt);
    assert(observation.completedAt < contract.deadline);
  }
  assert(association.before.completedAt <= association.after.startedAt);
  assert(association.before.requestId < association.after.requestId);
}

for (const mode of ['healthy', 'bare', 'pid-children']) {
  test(`startup ${mode}`, async () => fixture(mode, async ({owner, contract, calls, connections}) => {
    const originalSnapshot = contract.snapshot;
    contract.snapshot = async () => ({endpoint: {host: '127.0.0.1', port: (await originalSnapshot()).endpoint.port, browserPath: mode === 'bare' ? '/devtools/browser' : '/devtools/browser/owned'}, browserVersion: 'Chrome/131'});
    contract.validateSelectedDocument = async () => { throw new Error('Startup must not validate a document'); };
    const result = await associateBrowserProcess(owner, contract);
    assert.equal(result.kind, 'bounded-browser-process-association');
    assertAssociation(result, calls, contract);
    assert.equal(calls.length, 2);
    assert.equal(connections(), 1);
    assert.equal(result.observations, undefined);
  }));
}

for (const entry of [inspectTargets, associateBrowserProcess]) {
  for (const expectedBrowserPid of [undefined, '123', 0, -1, 1.2, 2147483648, Number.MAX_SAFE_INTEGER + 1, NaN, Infinity]) {
    test(`${entry.name} refuses native PID ${String(expectedBrowserPid)}`, async () => fixture('healthy', async ({owner, contract, calls, connections, failures}) => {
      await assert.rejects(entry(owner, {...contract, expectedBrowserPid}));
      await assert.rejects(entry(owner, contract));
      assert.equal(calls.length, 0);
      assert.equal(connections(), 0);
      assert.equal(failures(), 1);
    }));
  }
  for (const mode of ['pid-wrong', 'pid-stale', 'pid-after-late', 'final-disconnect']) {
    test(`${entry.name} shared final refusal ${mode}`, async () => fixture(mode, async ({owner, contract, calls}) => {
      let original;
      await assert.rejects(entry(owner, contract), (error) => { original = error; return true; });
      const count = calls.length;
      await assert.rejects(associateBrowserProcess(owner, contract), (error) => error === original);
      await assert.rejects(inspectTargets(owner, contract), (error) => error === original);
      assert.equal(calls.length, count);
    }));
  }
  test(`${entry.name} overlap cancels retained connection`, async () => fixture('healthy', async ({owner, contract, calls, failures}) => {
    const other = entry === inspectTargets ? associateBrowserProcess : inspectTargets;
    let entered;
    const ready = new Promise((resolve) => { entered = resolve; });
    let release;
    const blocked = new Promise((resolve) => { release = resolve; });
    const originalSnapshot = contract.snapshot;
    let snapshots = 0;
    contract.snapshot = async () => {
      if (++snapshots === 2) { entered(); await blocked; }
      return originalSnapshot();
    };
    const active = entry(owner, contract);
    const rejected = assert.rejects(active);
    await ready;
    const count = calls.length;
    let original;
    await assert.rejects(other(owner, contract), (error) => { original = error; return true; });
    await rejected;
    release();
    await assert.rejects(entry(owner, contract), (error) => error === original);
    await new Promise((resolve) => setTimeout(resolve, 10));
    assert.equal(calls.length, count);
    assert.equal(failures(), 1);
  }));
}

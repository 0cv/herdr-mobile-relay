'use strict';

const http = require('node:http');
const {randomUUID} = require('node:crypto');
const WebSocket = require('ws');

const EXPRESSION = `JSON.stringify({href:location.href,origin:location.origin,standalone:matchMedia('(display-mode: standalone)').matches || navigator.standalone === true,provider:matchMedia('(display-mode: standalone)').matches || navigator.standalone === true ? 'android-standalone' : 'browser',timeOrigin:performance.timeOrigin})`;
const LIMIT = 65536;
const owners = new WeakMap();

function record(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid protocol object');
  return value;
}
function text(value) {
  if (typeof value !== 'string' || !value || value.length > 8192) throw new Error('Invalid protocol string');
  return value;
}
function inventory(value) {
  const infos = record(value).targetInfos;
  if (!Array.isArray(infos) || !infos.length || infos.length > 32) throw new Error('Invalid target inventory');
  const ids = new Set();
  const result = infos.map((entry) => {
    record(entry);
    const targetId = text(entry.targetId);
    if (ids.has(targetId)) throw new Error('Duplicate target');
    ids.add(targetId);
    if (typeof entry.url !== 'string' || typeof entry.title !== 'string' || typeof entry.attached !== 'boolean') throw new Error('Malformed target');
    return {targetId, type: text(entry.type), url: entry.url, title: entry.title};
  });
  return result.sort((a, b) => a.targetId.localeCompare(b.targetId));
}

function processId(value) {
  if (!Number.isSafeInteger(value) || value <= 0 || value > 2147483647) throw new Error('Invalid browser process ID');
  return value;
}
function browserProcess(value, expected) {
  const entries = record(value).processInfo;
  if (!Array.isArray(entries) || !entries.length || entries.length > 256) throw new Error('Invalid process inventory');
  let browser;
  const ids = new Set();
  for (const entry of entries) {
    record(entry);
    const type = text(entry.type);
    const id = processId(entry.id);
    if (ids.has(id) || typeof entry.cpuTime !== 'number' || !Number.isFinite(entry.cpuTime) || entry.cpuTime < 0) throw new Error('Invalid process record');
    ids.add(id);
    if (type !== 'browser') continue;
    if (browser !== undefined) throw new Error('Duplicate browser process');
    browser = id;
  }
  if (browser !== expected) throw new Error('Browser process association mismatch');
  return browser;
}

async function inspect(owner, contract, targets) {
  let state = owners.get(owner);
  if (!state) { state = {busy: false}; owners.set(owner, state); }
  if (state.failed || state.busy) {
    const error = state.failed || new Error('Inspection owner quarantined or busy');
    state.failed = error;
    state.cancel?.(error);
    throw error;
  }
  state.busy = true;
  let socket;
  let request;
  let timer;
  let pending;
  let failed;
  let rejectFailure;
  let sequence = 0;
  let messages = 0;
  let connected = false;
  const connectionId = randomUUID();
  const failure = new Promise((_, reject) => { rejectFailure = reject; });
  failure.catch(() => {});
  const fail = (error) => {
    if (failed) return;
    failed = error instanceof Error ? error : new Error('Inspection failed');
    state.failed = failed;
    rejectFailure(failed);
    request?.destroy();
    socket?.terminate();
    try { contract.failure(failed); } catch { /* The original failure remains authoritative. */ }
  };
  state.cancel = fail;
  const check = () => {
    if (failed) throw failed;
    if (state.failed || !Number.isFinite(contract.deadline) || Date.now() >= contract.deadline) throw new Error('Inspection deadline or quarantine');
    contract.assertOwner(owner);
    if (failed || state.failed || Date.now() >= contract.deadline || (connected && socket.readyState !== WebSocket.OPEN)) throw new Error('Inspection refused');
  };
  const bounded = async (operation) => {
    check();
    const value = await Promise.race([operation(), failure]);
    check();
    return value;
  };
  const send = async (method, params = {}, sessionId) => bounded(() => new Promise((resolve) => {
    check();
    if (pending || ++sequence > 400) throw new Error('Request limit');
    pending = {id: sequence, sessionId, resolve, method, params};
    socket.send(JSON.stringify({id: sequence, method, params, ...(sessionId ? {sessionId} : {})}), (error) => { if (error) fail(error); });
  }));
  const snapshot = () => bounded(() => contract.snapshot(owner));
  try {
    check();
    const expectedBrowserPid = processId(contract.expectedBrowserPid);
    if (state.expectedBrowserPid !== undefined && state.expectedBrowserPid !== expectedBrowserPid) throw new Error('Original browser process changed');
    state.expectedBrowserPid = expectedBrowserPid;
    timer = setTimeout(() => fail(new Error('Inspection deadline')), Math.min(contract.deadline - Date.now(), 2147483647));
    const before = await snapshot();
    await bounded(() => contract.validateSnapshot(owner, before, before));
    const endpoint = record(before.endpoint);
    if (endpoint.host !== '127.0.0.1' || !Number.isInteger(endpoint.port) || endpoint.port < 1 || endpoint.port > 65535 || !/^\/devtools\/browser(?:\/[A-Za-z0-9-]+)?$/.test(endpoint.browserPath)) throw new Error('Invalid owner endpoint');
    const expected = `ws://127.0.0.1:${endpoint.port}${endpoint.browserPath}`;
    const version = await bounded(() => new Promise((resolve, reject) => {
      request = http.get({hostname: endpoint.host, port: endpoint.port, path: '/json/version', agent: false, maxHeaderSize: 8192}, (response) => {
        let size = 0;
        const chunks = [];
        response.on('error', reject);
        response.on('aborted', () => reject(new Error('Partial discovery body')));
        if (response.statusCode !== 200) { response.destroy(); reject(new Error('Discovery status')); return; }
        response.on('data', (chunk) => {
          size += chunk.length;
          if (size > LIMIT) { response.destroy(); reject(new Error('Discovery body limit')); return; }
          chunks.push(chunk);
        });
        response.on('end', () => {
          try { resolve(record(JSON.parse(Buffer.concat(chunks).toString('utf8')))); } catch (error) { reject(error); }
        });
      });
      request.on('error', reject);
    }));
    if (version.webSocketDebuggerUrl !== expected || version.Browser !== before.browserVersion) throw new Error('Browser endpoint association mismatch');
    await bounded(() => contract.validateSnapshot(owner, before, before));
    await bounded(() => new Promise((resolve, reject) => {
      socket = new WebSocket(expected, {maxPayload: LIMIT, perMessageDeflate: false, followRedirects: false, handshakeTimeout: Math.max(1, contract.deadline - Date.now())});
      socket.on('error', (error) => { reject(error); fail(error); });
      socket.on('close', () => fail(new Error('Inspection socket disconnected')));
      socket.on('message', (data, binary) => {
        try {
          check();
          if (binary || data.length > LIMIT || ++messages > 400) throw new Error('Message limit or binary frame');
          const message = record(JSON.parse(data.toString('utf8')));
          if (message.method === 'Target.attachedToTarget' && pending?.method === 'Target.attachToTarget' && !pending.attached && message.id === undefined && message.sessionId === undefined) {
            const event = record(message.params);
            if (record(event.targetInfo).targetId !== pending.params.targetId || event.waitingForDebugger !== false) throw new Error('Wrong attached target');
            pending.attached = text(event.sessionId);
            return;
          }
          if (!pending || message.id !== pending.id || message.sessionId !== pending.sessionId || message.method !== undefined || message.error !== undefined) throw new Error('Uncorrelated CDP reply or event');
          const result = record(message.result);
          if (pending.attached && pending.attached !== result.sessionId) throw new Error('Wrong attached session');
          const resolveReply = pending.resolve;
          pending = undefined;
          resolveReply(result);
        } catch (error) { fail(error); }
      });
      socket.once('open', resolve);
    }));
    connected = true;
    const observePid = async () => {
      const startedAt = Date.now();
      const requestId = sequence + 1;
      const pid = browserProcess(await send('SystemInfo.getProcessInfo'), expectedBrowserPid);
      return {pid, requestId, connectionId, startedAt, completedAt: Date.now()};
    };
    const pidBefore = await observePid();
    let result;
    if (targets) {
      const initial = inventory(await send('Target.getTargets'));
      const handles = before.handles;
      const pages = initial.filter((target) => target.type === 'page');
      if (!Array.isArray(handles) || handles.some((handle) => typeof handle !== 'string') || new Set(handles).size !== handles.length || JSON.stringify([...handles].sort()) !== JSON.stringify(pages.map((target) => target.targetId).sort()) || !handles.includes(before.selectedHandle)) throw new Error('Unmapped target or selected handle');
      const sessions = new Set();
      const observations = [];
      const observe = async (sessionId) => {
        const first = record((await send('DOM.getDocument', {depth: 0, pierce: false}, sessionId)).root);
        const reply = await send('Runtime.evaluate', {expression: EXPRESSION, returnByValue: true, silent: true, throwOnSideEffect: true, timeout: Math.max(1, contract.deadline - Date.now())}, sessionId);
        const remote = record(reply.result);
        if (reply.exceptionDetails || remote.type !== 'string') throw new Error('Document observation unavailable');
        const observation = record(JSON.parse(text(remote.value)));
        if (typeof observation.href !== 'string' || typeof observation.origin !== 'string' || typeof observation.standalone !== 'boolean' || observation.provider !== (observation.standalone ? 'android-standalone' : 'browser') || !Number.isFinite(observation.timeOrigin)) throw new Error('Malformed observation');
        const last = record((await send('DOM.getDocument', {depth: 0, pierce: false}, sessionId)).root);
        if (!Number.isSafeInteger(first.backendNodeId) || first.backendNodeId <= 0 || first.nodeType !== 9 || first.backendNodeId !== last.backendNodeId || last.nodeType !== 9 || first.documentURL !== observation.href || last.documentURL !== observation.href) throw new Error('Document changed');
        return {...observation, backendNodeId: first.backendNodeId};
      };
      for (const target of pages) {
        const attached = await send('Target.attachToTarget', {targetId: target.targetId, flatten: true});
        const sessionId = text(attached.sessionId);
        if (sessions.has(sessionId)) throw new Error('Duplicate target session');
        sessions.add(sessionId);
        observations.push({targetId: target.targetId, sessionId, document: await observe(sessionId)});
      }
      for (const observation of observations) {
        if (JSON.stringify(await observe(observation.sessionId)) !== JSON.stringify(observation.document)) throw new Error('Observation changed within bracket');
      }
      if (JSON.stringify(inventory(await send('Target.getTargets'))) !== JSON.stringify(initial)) throw new Error('Inventory changed');
      result = {kind: 'bounded-nonactivating-observation', targets: initial, observations: observations.map(({targetId, document}) => ({targetId, document})), selectedHandle: before.selectedHandle};
    }
    const pidAfter = await observePid();
    const processAssociation = {kind: 'bounded-sequential-service-to-process', before: pidBefore, after: pidAfter};
    result = targets ? {...result, processAssociation} : {kind: 'bounded-browser-process-association', processAssociation};
    const after = await snapshot();
    await bounded(() => contract.validateSnapshot(owner, before, after));
    if (targets) await bounded(() => contract.validateSelectedDocument(owner, result, before, after));
    check();
    if (pending) throw new Error('Abandoned inspection response');
    socket.removeAllListeners('close');
    socket.terminate();
    return result;
  } catch (error) {
    fail(error);
    throw failed;
  } finally {
    clearTimeout(timer);
    request?.destroy();
    socket?.terminate();
    state.busy = false;
    state.cancel = undefined;
  }
}

function inspectTargets(owner, contract) {
  return inspect(owner, contract, true);
}
function associateBrowserProcess(owner, contract) {
  return inspect(owner, contract, false);
}

module.exports = {inspectTargets, associateBrowserProcess};

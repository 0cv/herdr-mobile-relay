'use strict';

const http = require('node:http');
const {randomUUID} = require('node:crypto');
const WebSocket = require('ws');

const CORE_EXPRESSION = `JSON.stringify({href:location.href,origin:location.origin,timeOrigin:performance.timeOrigin})`;
const LIMIT = 65536;
const REMOTE_TYPES = new Set(['bigint', 'boolean', 'function', 'number', 'object', 'string', 'symbol', 'undefined']);
const EXCEPTION_CLASSES = new Set(['AggregateError', 'Error', 'EvalError', 'RangeError', 'ReferenceError', 'SyntaxError', 'TypeError', 'URIError']);
const SIDE_EFFECT_DESCRIPTION = 'EvalError: Possible side-effect in debug-evaluate';
const SIDE_EFFECT_STACK = /^(?:\n\x20{4}at [^\r\n]{1,512}){1,64}$/u;
const KNOWN_METHODS = new Set([
  'SystemInfo.getProcessInfo', 'Target.getTargets', 'Target.attachToTarget', 'Target.attachedToTarget',
  'Target.targetCreated', 'Target.targetInfoChanged', 'Target.targetDestroyed', 'Target.detachedFromTarget',
  'DOM.getDocument', 'DOM.documentUpdated', 'DOM.childNodeCountUpdated', 'DOM.scrollableFlagUpdated',
  'DOM.topLayerElementsUpdated', 'Runtime.evaluate',
]);
const DIAGNOSTIC_INTEGER_MAX = 2147483647;
const DIAGNOSTIC_REQUEST_MAX = 400;
const owners = new WeakMap();

function remoteType(value) {
  return typeof value === 'string' && REMOTE_TYPES.has(value) ? value : 'unknown';
}
function isSideEffectDescription(value) {
  if (typeof value !== 'string' || value.length > LIMIT) return false;
  if (value === SIDE_EFFECT_DESCRIPTION) return true;
  return value.startsWith(`${SIDE_EFFECT_DESCRIPTION}\n`) && SIDE_EFFECT_STACK.test(value.slice(SIDE_EFFECT_DESCRIPTION.length));
}
function exceptionInfo(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value) || !value.exception || typeof value.exception !== 'object' || Array.isArray(value.exception)) {
    return {className: 'unknown', cause: 'unknown'};
  }
  const className = typeof value.exception.className === 'string' && EXCEPTION_CLASSES.has(value.exception.className) ? value.exception.className : 'unknown';
  return {className, cause: className === 'EvalError' && isSideEffectDescription(value.exception.description) ? 'known-side-effect-rejection' : 'unknown'};
}
function observationDiagnostic(reply, remote, targetOrdinal, observationPass) {
  const exception = exceptionInfo(reply.exceptionDetails);
  return {
    failurePredicate: 'document-observation-unavailable', targetOrdinal, observationPass,
    exceptionDetails: Boolean(reply.exceptionDetails), exceptionClass: exception.className, exceptionCause: exception.cause,
    remoteType: remoteType(remote.type),
  };
}
function knownMethod(value) {
  return typeof value === 'string' && KNOWN_METHODS.has(value) ? value : 'unknown';
}
function diagnosticId(message) {
  if (!Object.hasOwn(message, 'id')) return {idPresence: 'absent', idType: 'absent', idValue: 'none'};
  const value = message.id;
  if (typeof value === 'number') {
    return {idPresence: 'present', idType: 'number', idValue: Number.isSafeInteger(value) && value >= 1 && value <= DIAGNOSTIC_REQUEST_MAX ? value : 'nonmatching'};
  }
  const type = value === null ? 'null' : Array.isArray(value) ? 'array' : typeof value;
  return {idPresence: 'present', idType: ['boolean', 'object', 'string'].includes(type) ? type : 'other', idValue: 'nonmatching'};
}
function diagnosticInteger(value) {
  return Number.isSafeInteger(value) && value >= 0 && value <= DIAGNOSTIC_INTEGER_MAX ? value : 'nonmatching';
}
function diagnosticEventParams(method, params) {
  if (!params || typeof params !== 'object' || Array.isArray(params)) return undefined;
  if (method === 'DOM.childNodeCountUpdated') return {nodeId: diagnosticInteger(params.nodeId), childNodeCount: diagnosticInteger(params.childNodeCount)};
  if (method === 'DOM.scrollableFlagUpdated') return {nodeId: diagnosticInteger(params.nodeId), isScrollable: typeof params.isScrollable === 'boolean' ? params.isScrollable : 'nonmatching'};
  return undefined;
}
function messageClassification(message) {
  const hasId = Object.hasOwn(message, 'id');
  const hasMethod = Object.hasOwn(message, 'method');
  if (hasMethod && !hasId && typeof message.method === 'string') return 'event';
  if (!hasMethod && hasId) return 'reply';
  return 'other';
}
function selectedTopLayerNotification(message, phase, pending, sessionTargets, selectedHandle) {
  if (phase !== 'post-cdp-completion' || pending || message.method !== 'DOM.topLayerElementsUpdated'
    || Object.hasOwn(message, 'id') || Object.hasOwn(message, 'result') || Object.hasOwn(message, 'error')) return false;
  const fields = Object.keys(message);
  const validFields = Object.hasOwn(message, 'params')
    ? fields.length === 3 && ['method', 'params', 'sessionId'].every((field) => fields.includes(field))
    : fields.length === 2 && ['method', 'sessionId'].every((field) => fields.includes(field));
  if (!validFields) return false;
  const association = typeof message.sessionId === 'string' ? sessionTargets.get(message.sessionId) : undefined;
  if (!association || association.targetId !== selectedHandle) return false;
  if (!Object.hasOwn(message, 'params')) return true;
  const params = message.params;
  return Boolean(params && typeof params === 'object' && !Array.isArray(params) && Object.keys(params).length === 0);
}
function diagnosticSession(message, sessionTargets) {
  if (!Object.hasOwn(message, 'sessionId')) return {sessionRelation: 'root', sessionOrdinal: 'none', targetOrdinal: 'none'};
  const association = typeof message.sessionId === 'string' ? sessionTargets.get(message.sessionId) : undefined;
  if (!association) return {sessionRelation: 'unknown', sessionOrdinal: 'unknown', targetOrdinal: 'unknown'};
  return {sessionRelation: 'known-local-ordinal', sessionOrdinal: association.sessionOrdinal, targetOrdinal: association.targetOrdinal};
}
function pendingSessionOrdinal(value, sessionTargets) {
  if (value.sessionId === undefined) return 'root';
  return sessionTargets.get(value.sessionId)?.sessionOrdinal ?? 'unknown';
}
function uncorrelatedDiagnostic(message, pending, sessionTargets, sequence, phase, messages) {
  const classification = messageClassification(message);
  const method = knownMethod(message.method);
  const params = classification === 'event' ? diagnosticEventParams(method, message.params) : undefined;
  return {
    failurePredicate: 'uncorrelated-cdp-message', classification, method,
    ...diagnosticId(message), ...diagnosticSession(message, sessionTargets),
    lastSequence: sequence,
    pendingId: pending ? pending.id : 'none',
    pendingMethod: pending ? knownMethod(pending.method) : 'none',
    pendingSessionOrdinal: pending ? pendingSessionOrdinal(pending, sessionTargets) : 'none',
    phase, messageCount: messages,
    ...(params ? {params} : {}),
  };
}

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
  let phase = 'initial';
  const sessionTargets = new Map();
  let selectedHandle;
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
    selectedHandle = before.selectedHandle;
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
          if (++messages > 400 || binary || data.length > LIMIT) throw new Error('Message limit or binary frame');
          const message = record(JSON.parse(data.toString('utf8')));
          if (selectedTopLayerNotification(message, phase, pending, sessionTargets, selectedHandle)) return;
          if (message.method === 'Target.attachedToTarget' && pending?.method === 'Target.attachToTarget' && !pending.attached && message.id === undefined && message.sessionId === undefined) {
            const event = record(message.params);
            if (record(event.targetInfo).targetId !== pending.params.targetId || event.waitingForDebugger !== false) throw new Error('Wrong attached target');
            pending.attached = text(event.sessionId);
            return;
          }
          if (!pending || message.id !== pending.id || message.sessionId !== pending.sessionId || message.method !== undefined || message.error !== undefined) {
            throw new Error(`Uncorrelated CDP reply or event (${JSON.stringify(uncorrelatedDiagnostic(message, pending, sessionTargets, sequence, phase, messages))})`);
          }
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
      const observations = [];
      const observe = async (sessionId, targetOrdinal, observationPass) => {
        const first = record((await send('DOM.getDocument', {depth: 0, pierce: false}, sessionId)).root);
        const reply = await send('Runtime.evaluate', {expression: CORE_EXPRESSION, returnByValue: true, silent: true, throwOnSideEffect: true, timeout: Math.max(1, contract.deadline - Date.now())}, sessionId);
        const remote = record(reply.result);
        const diagnostic = observationDiagnostic(reply, remote, targetOrdinal, observationPass);
        if (reply.exceptionDetails || remote.type !== 'string') throw new Error(`Document observation unavailable (${JSON.stringify(diagnostic)})`);
        const observation = record(JSON.parse(text(remote.value)));
        if (JSON.stringify(Object.keys(observation).sort()) !== JSON.stringify(['href', 'origin', 'timeOrigin'])) throw new Error('Malformed core observation');
        if (typeof observation.href !== 'string' || typeof observation.origin !== 'string' || !Number.isFinite(observation.timeOrigin)) throw new Error('Malformed core observation');
        const last = record((await send('DOM.getDocument', {depth: 0, pierce: false}, sessionId)).root);
        if (!Number.isSafeInteger(first.backendNodeId) || first.backendNodeId <= 0 || first.nodeType !== 9 || first.backendNodeId !== last.backendNodeId || last.nodeType !== 9 || first.documentURL !== observation.href || last.documentURL !== observation.href) throw new Error('Document changed');
        return {href: observation.href, origin: observation.origin, timeOrigin: observation.timeOrigin, backendNodeId: first.backendNodeId};
      };
      for (const [index, target] of pages.entries()) {
        const attached = await send('Target.attachToTarget', {targetId: target.targetId, flatten: true});
        const sessionId = text(attached.sessionId);
        if (sessionTargets.has(sessionId)) throw new Error('Duplicate target session');
        sessionTargets.set(sessionId, {sessionOrdinal: index + 1, targetOrdinal: index + 1, targetId: target.targetId});
        observations.push({targetId: target.targetId, sessionId, document: await observe(sessionId, index + 1, 'initial')});
      }
      phase = 'repeat';
      for (const [index, observation] of observations.entries()) {
        if (JSON.stringify(await observe(observation.sessionId, index + 1, 'repeat')) !== JSON.stringify(observation.document)) throw new Error('Observation changed within bracket');
      }
      phase = 'final-snapshot';
      if (JSON.stringify(inventory(await send('Target.getTargets'))) !== JSON.stringify(initial)) throw new Error('Inventory changed');
      result = {kind: 'bounded-nonactivating-core-observation', targets: initial, observations: observations.map(({targetId, document}) => ({targetId, document})), selectedHandle: before.selectedHandle};
    }
    phase = 'final-snapshot';
    const pidAfter = await observePid();
    if (targets) phase = 'post-cdp-completion';
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

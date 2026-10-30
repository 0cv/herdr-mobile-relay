import { createServer, createConnection } from 'node:net';
import { lstat, chmod, unlink } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { performance } from 'node:perf_hooks';

export const ACTIVITY_LIMITS = Object.freeze({ pageEntries: 200, traversal: 2048, frameBytes: 1024 * 1024, textBytes: 64 * 1024, toolBytes: 16 * 1024, tools: 64, cursors: 64, streams: 8, pages: 8, pending: 16, handshakeMs: 1000, cadenceMs: 250 });
const clean = value => typeof value === 'string' ? value.replace(/\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|$))|[\x00-\x08\x0b-\x1f\x7f]/g, '') : '';
function bounded(value, bytes, omitted = { truncated: false }) {
  const original = typeof value === 'string' ? value : '';
  if (original.length > bytes * 2) omitted.truncated = true;
  const text = clean(original.slice(0, bytes * 2));
  const encoded = Buffer.from(text);
  if (encoded.length <= bytes) return text;
  omitted.truncated = true;
  return encoded.subarray(0, bytes).toString('utf8').replace(/\ufffd$/, '');
}
function input(value, omitted, depth = 0, budget = { remaining: 256 }) {
  if (depth > 3 || --budget.remaining < 0) { omitted.truncated = true; return '[omitted]'; }
  if (typeof value === 'string') return bounded(value, 2048, omitted);
  if (typeof value === 'number' || typeof value === 'boolean' || value === null) return value;
  if (Array.isArray(value)) {
    if (value.length > 32) omitted.truncated = true;
    return value.slice(0, 32).map(item => input(item, omitted, depth + 1, budget));
  }
  if (!value || typeof value !== 'object') return undefined;
  const result = {};
  let count = 0;
  for (const key in value) {
    if (!Object.hasOwn(value, key)) continue;
    if (++count > 32 || budget.remaining <= 0) { omitted.truncated = true; break; }
    Object.defineProperty(result, bounded(key, 160, omitted), { value: input(value[key], omitted, depth + 1, budget), enumerable: true });
  }
  return result;
}
function visibleText(content, bytes = ACTIVITY_LIMITS.textBytes, omitted = { truncated: false }) {
  if (typeof content === 'string') return bounded(content, bytes, omitted);
  if (!Array.isArray(content)) return '';
  if (content.length > 128) omitted.truncated = true;
  let result = '';
  for (const block of content.slice(0, 128)) {
    if (block?.type !== 'text') continue;
    result = bounded(result + (result ? '\n' : '') + bounded(block.text, bytes, omitted), bytes, omitted);
  }
  return result;
}
function projectTool(id, name, args, result) {
  const omitted = { truncated: false };
  const tool = {
    id: bounded(id, 256, omitted), name: bounded(name, 160, omitted),
    input: bounded(JSON.stringify(input(args, omitted)), ACTIVITY_LIMITS.toolBytes, omitted),
  };
  const outputOmitted = { truncated: false };
  let output = visibleText(result?.content, ACTIVITY_LIMITS.toolBytes, outputOmitted);
  const blocks = Array.isArray(result?.content) ? result.content.slice(0, 128) : [];
  const notice = blocks.some(block => block?.type === 'image') ? 'Image tool output omitted from this conversation view.'
    : blocks.some(block => block?.type !== 'text') ? 'Unsupported tool output omitted from this conversation view.' : '';
  if (notice) output = bounded(output, ACTIVITY_LIMITS.toolBytes - Buffer.byteLength(notice) - 1, outputOmitted) + (output ? '\n' : '') + notice;
  if (!output && outputOmitted.truncated) output = 'Tool output omitted by relay limits.';
  omitted.truncated ||= outputOmitted.truncated;
  if (output) tool.output = output;
  if (result) tool.state = result.isError ? 'failed' : 'completed';
  if (result?.isError) tool.error = true;
  if (omitted.truncated) tool.truncated = true;
  return tool;
}
export function activityEntry(entry, manager, results = new Map()) {
  if (entry?.type === 'custom_message') {
    if (entry.display !== true) return null;
    const omitted = { truncated: false };
    const text = visibleText(entry.content, ACTIVITY_LIMITS.textBytes, omitted).trim() || 'Extension message has no displayable text.';
    return { id: `pi:${entry.id}`, kind: 'notice', timestamp: typeof entry.timestamp === 'string' ? entry.timestamp : '', role: 'assistant', text, truncated: omitted.truncated };
  }
  if (entry?.type === 'compaction') return { id: `pi:${entry.id}`, kind: 'compaction', timestamp: entry.timestamp || '', role: 'assistant', text: 'Context compacted — earlier recorded messages remain available.' };
  if (entry?.type !== 'message' || !['user', 'assistant'].includes(entry.message?.role)) return null;
  const message = entry.message;
  const omitted = { truncated: false };
  let text = visibleText(message.content, ACTIVITY_LIMITS.textBytes, omitted);
  const userBlocks = message.role === 'user' && Array.isArray(message.content) ? message.content.slice(0, 128) : [];
  const userOmission = userBlocks.some(block => block?.type === 'image') ? 'Image content omitted from this conversation view.'
    : userBlocks.some(block => block?.type !== 'text') ? 'Unsupported user content omitted from this conversation view.' : '';
  if (userOmission) text = bounded(text, ACTIVITY_LIMITS.textBytes - Buffer.byteLength(userOmission) - 1, omitted) + (text ? '\n' : '') + userOmission;
  if (!text && omitted.truncated) text = 'Message content omitted by relay limits.';
  const tools = [];
  if (message.role === 'assistant' && Array.isArray(message.content)) {
    for (const block of message.content.slice(0, 128)) {
      if (block?.type !== 'toolCall' || typeof block.id !== 'string' || typeof block.name !== 'string') continue;
      const result = manager?.getEntry(results.get(block.id))?.message;
      if (tools.length === ACTIVITY_LIMITS.tools) { omitted.truncated = true; break; }
      tools.push(projectTool(block.id, block.name, block.arguments, result?.role === 'toolResult' && result.toolCallId === block.id ? result : undefined));
    }
  }
  if (!text && !tools.length) return null;
  return { id: `pi:${entry.id}`, timestamp: typeof entry.timestamp === 'string' ? entry.timestamp : '', role: message.role, text, ...(tools.length ? { tools } : {}), truncated: omitted.truncated || tools.some(tool => tool.truncated) };
}

export function createActivitySource(manager, { incomplete = false } = {}) {
  const incarnation = randomUUID();
  let epoch = 1;
  let sequence = 0;
  let provisional = 0;
  let activeId;
  let activeEntry;
  let toolEntryId;
  let toolAssistantId;
  const liveTools = new Map();
  let liveToolBytes = 0;
  let toolsDirty = false;
  let dirty = new Map();
  let removed = new Set();
  let reset = false;
  let statusDirty = false;
  let closed = false;
  let observedLeaf = manager.getLeafId();
  let capturedPrompt;
  let pendingCommits = [];
  let incompleteTurn = false;
  let lateJoin = incomplete;
  const cursors = new Map();
  const listeners = new Set();
  const capabilities = { source: 'pi_events', active_branch: true };
  let state = incomplete ? 'incomplete' : 'ready';
  function revision() { return `${incarnation}:${epoch}`; }
  function reconcile(captureNewPrompt = false) {
    if (!pendingCommits.length && !captureNewPrompt && manager.getLeafId() === observedLeaf) return;
    let recoveredBoundary = pendingCommits.length > 0;
    const results = new Map();
    const finalized = new Map();
    for (const boundary of pendingCommits) {
      const assistant = manager.getEntry(boundary.messageEntryId);
      if (assistant?.type !== 'message' || assistant.message?.role !== 'assistant') { reset = true; state = 'incomplete'; incompleteTurn = true; recoveredBoundary = false; }
      else {
        finalized.set(assistant.id, assistant);
        if (['error', 'aborted'].includes(assistant.message.stopReason)) incompleteTurn = true;
      }
      if (boundary.liveId) { removed.add(boundary.liveId); dirty.delete(boundary.liveId); if (activeId === boundary.liveId) { activeId = undefined; activeEntry = undefined; } }
      if (boundary.toolLiveId) {
        removed.add(boundary.toolLiveId); dirty.delete(boundary.toolLiveId);
        if (toolEntryId === boundary.toolLiveId) { toolEntryId = undefined; toolAssistantId = undefined; liveTools.clear(); liveToolBytes = 0; toolsDirty = false; }
      }
      for (const id of boundary.toolResultEntryIds) {
        const entry = manager.getEntry(id);
        if (entry?.type === 'message' && entry.message?.role === 'toolResult' && results.size < 256) results.set(entry.message.toolCallId, id);
      }
    }
    pendingCommits = [];
    const added = [];
    let next = manager.getLeafId();
    for (let count = 0; next && next !== observedLeaf && count < ACTIVITY_LIMITS.pageEntries; count++) {
      const entry = manager.getEntry(next);
      if (!entry) break;
      added.unshift(entry);
      next = entry.parentId;
    }
    if (next !== observedLeaf && next) { reset = true; state = 'incomplete'; incompleteTurn = true; recoveredBoundary = false; }
    observedLeaf = manager.getLeafId();
    for (const entry of finalized.values()) if (!added.some(item => item.id === entry.id)) queue(activityEntry(entry, manager, results));
    for (const entry of added) queue(activityEntry(entry, manager, results));
    if (recoveredBoundary) lateJoin = false;
    if (lateJoin || incompleteTurn) state = 'incomplete';
  }
  function capturePrompt() {
    const leaf = manager.getLeafId();
    if (!leaf || leaf === observedLeaf || leaf === capturedPrompt) return;
    const entry = manager.getEntry(leaf);
    if (entry?.type !== 'message' || entry.message?.role !== 'user') return;
    capturedPrompt = leaf;
    queue(activityEntry(entry, manager));
  }
  function emit() {
    if (closed) return;
    reconcile();
    capturePrompt();
    if (toolsDirty && toolEntryId) { toolsDirty = false; queue(toolEntry()); }
    if ( (!dirty.size && !removed.size && !reset && !statusDirty)) return;
    const frame = { kind: reset ? 'reset' : 'batch', incarnation, epoch, sequence: ++sequence, source_revision: revision(), entries: [...dirty.values()], remove: [...removed], state, capabilities };
    if (Buffer.byteLength(JSON.stringify(frame)) > ACTIVITY_LIMITS.frameBytes - 8192) {
      frame.kind = 'reset'; frame.entries = []; frame.remove = []; frame.state = 'incomplete';
    }
    dirty = new Map(); removed = new Set(); reset = false; statusDirty = false;
    for (const listener of listeners) listener(frame);
  }
  const timer = setInterval(emit, ACTIVITY_LIMITS.cadenceMs);
  timer.unref?.();
  function clearLive() {
    for (const id of dirty.keys()) if (id.startsWith('live:')) dirty.delete(id);
    if (activeId) removed.add(activeId);
    if (toolEntryId) removed.add(toolEntryId);
    activeId = undefined;
    activeEntry = undefined;
    toolEntryId = undefined;
    toolAssistantId = undefined;
    liveTools.clear(); liveToolBytes = 0; toolsDirty = false;
  }
  function toolEntry() { return { id: toolEntryId, role: 'assistant', timestamp: '', text: '', tools: [...liveTools.values()] }; }
  function updateTool(type, event) {
    if (typeof event.toolCallId !== 'string' || typeof event.toolName !== 'string') return;
    if (!liveTools.has(event.toolCallId) && liveTools.size >= ACTIVITY_LIMITS.tools) { reset = true; state = 'incomplete'; incompleteTurn = true; statusDirty = true; return; }
    if (!toolAssistantId) {
      let next = manager.getLeafId();
      for (let count = 0; next && count < ACTIVITY_LIMITS.pageEntries; count++) {
        const entry = manager.getEntry(next);
        if (!entry) break;
        if (entry.type === 'message' && entry.message.role === 'assistant' && entry.message.content?.some?.(block => block.type === 'toolCall' && [event.toolCallId, event.parentToolCallId].includes(block.id))) {
          toolAssistantId = entry.id;
          const projected = activityEntry(entry, manager);
          if (projected?.text) { activeId ||= `live:${incarnation}:${++provisional}`; activeEntry = { ...projected, id: activeId, tools: undefined }; queue(activeEntry); }
          break;
        }
        next = entry.parentId;
      }
    }
    toolEntryId ||= `live:${incarnation}:tools:${++provisional}`;
    const previous = liveTools.get(event.toolCallId);
    const result = type === 'tool_execution_update' ? event.partialResult : type === 'tool_execution_end' ? { content: event.result?.content, isError: event.isError } : undefined;
    const tool = projectTool(event.toolCallId, event.toolName, event.args, result);
    if (event.args === undefined && previous?.input !== undefined) { tool.input = previous.input; if (previous.truncated) tool.truncated = true; }
    if (event.parentToolCallId || previous?.parent_id) {
      const omitted = { truncated: false };
      tool.parent_id = bounded(event.parentToolCallId || previous.parent_id, 256, omitted);
      if (omitted.truncated) tool.truncated = true;
    }
    tool.state = type === 'tool_execution_end' ? event.isError ? 'failed' : 'completed' : 'running';
    const bytes = Buffer.byteLength(JSON.stringify(tool));
    const nextBytes = liveToolBytes + bytes - (previous ? Buffer.byteLength(JSON.stringify(previous)) : 0);
    if (nextBytes > ACTIVITY_LIMITS.frameBytes / 2 - 8192) {
      reset = true; state = 'incomplete'; incompleteTurn = true; statusDirty = true; return;
    }
    liveTools.set(event.toolCallId, tool); liveToolBytes = nextBytes; toolsDirty = true;
    state = lateJoin || incompleteTurn ? 'incomplete' : 'generating';
  }
  function queue(entry) {
    if (!entry) return;
    dirty.set(entry.id, entry);
    if (dirty.size > ACTIVITY_LIMITS.pageEntries || [...dirty.values()].reduce((size, item) => size + Buffer.byteLength(JSON.stringify(item)), 0) > ACTIVITY_LIMITS.frameBytes - 8192) {
      dirty.clear(); removed.clear(); reset = true; state = 'incomplete'; incompleteTurn = true;
    }
  }
  function page(cursor = '', limit = ACTIVITY_LIMITS.pageEntries) {
    if (closed) throw new Error('Activity source closed');
    emit();
    const now = Date.now();
    for (const [key, value] of cursors) if (value.expires < now) cursors.delete(key);
    const prior = cursor ? cursors.get(cursor) : null;
    if (cursor && (!prior || prior.epoch !== epoch)) throw new Error('Activity cursor expired');
    let next = prior ? prior.next : manager.getLeafId();
    const snapshot = prior?.snapshot || `${revision()}:${next || 'root'}`;
    const results = new Map(prior?.results || []);
    const entries = [];
    const provisional = cursor ? [] : [...(activeEntry ? [activeEntry] : []), ...(toolEntryId ? [toolEntry()] : [])];
    let bytes = Buffer.byteLength(JSON.stringify(provisional));
    let scanned = 0;
    let truncated = false;
    const seen = new Set();
    while (next && entries.length < Math.max(1, Math.min(ACTIVITY_LIMITS.pageEntries, limit)) && scanned++ < ACTIVITY_LIMITS.traversal) {
      if (seen.has(next)) { next = null; truncated = true; break; }
      seen.add(next);
      const entry = manager.getEntry(next);
      if (!entry) { next = null; truncated = true; break; }
      if (entry.type === 'message' && entry.message?.role === 'toolResult' && results.size < ACTIVITY_LIMITS.tools) results.set(entry.message.toolCallId, entry.id);
      const projected = entry.id === toolAssistantId ? null : activityEntry(entry, manager, results);
      if (projected) {
        const size = Buffer.byteLength(JSON.stringify(projected));
        if (size + bytes > ACTIVITY_LIMITS.frameBytes - 16384) {
          if (entries.length) break;
          projected.tools = []; projected.text = bounded(projected.text, ACTIVITY_LIMITS.textBytes); projected.truncated = true;
        }
        bytes += Buffer.byteLength(JSON.stringify(projected)); entries.unshift(projected);
        for (const tool of projected.tools || []) results.delete(tool.id);
      }
      next = entry.parentId;
    }
    if (scanned >= ACTIVITY_LIMITS.traversal && next) truncated = true;
    let nextCursor = '';
    if (next) {
      if (cursors.size >= ACTIVITY_LIMITS.cursors) cursors.delete(cursors.keys().next().value);
      nextCursor = randomUUID();
      cursors.set(nextCursor, { next, snapshot, epoch, results: [...results], expires: now + 15 * 60_000 });
    }
    return { available: true, entries, provisional, has_more: Boolean(next), next_cursor: nextCursor ? `pa:${nextCursor}` : '', state: 'ready', mode: 'snapshot', source_revision: revision(), snapshot_id: snapshot, total: null, diagnostics: { source_truncated: truncated }, sequence, capabilities, live_state: state };
  }
  return {
    incarnation,
    info() { return { incarnation, epoch, sequence, source_revision: revision(), state, capabilities }; },
    page(cursor, limit) { return page(cursor?.replace(/^pa:/, ''), limit); },
    listen(listener) { listeners.add(listener); return () => listeners.delete(listener); },
    event(type, event, ctx) {
      if (closed) return;
      if (ctx && ctx.sessionManager.getSessionId() !== manager.getSessionId()) { state = 'incomplete'; incompleteTurn = true; reset = true; emit(); return; }
      if (type === 'session_tree') {
        epoch++; sequence = 0; cursors.clear(); dirty.clear(); removed.clear(); activeId = undefined; activeEntry = undefined; toolEntryId = undefined; toolAssistantId = undefined; liveTools.clear(); liveToolBytes = 0; toolsDirty = false;
        observedLeaf = manager.getLeafId(); capturedPrompt = undefined; pendingCommits = []; incompleteTurn = false; lateJoin = false; state = 'ready'; reset = true; emit(); return;
      }
      if (type === 'message_start' && event.message?.role === 'assistant') {
        reconcile(true);
        clearLive(); activeId = `live:${incarnation}:${++provisional}`; incompleteTurn = false; lateJoin = false; state = 'generating'; statusDirty = true;
      }
      if (type === 'message_update' && activeId && event.message?.role === 'assistant') {
        const omitted = { truncated: false };
        let text = visibleText(event.message.content, ACTIVITY_LIMITS.textBytes, omitted);
        if (omitted.truncated) { incompleteTurn = true; state = 'incomplete'; statusDirty = true; if (!text) text = 'Message content omitted by relay limits.'; }
        if (text) { activeEntry = { id: activeId, role: 'assistant', timestamp: '', text, truncated: omitted.truncated }; queue(activeEntry); }
      }
      if (['tool_execution_start', 'tool_execution_update', 'tool_execution_end'].includes(type)) updateTool(type, event);
      if (type === 'turn_end') {
        pendingCommits.push({ messageEntryId: event.messageEntryId, liveId: activeId, toolLiveId: toolEntryId, toolResultEntryIds: (event.toolResultEntryIds || []).slice(0, ACTIVITY_LIMITS.tools) });
        if (pendingCommits.length > 64) { pendingCommits = []; reset = true; state = 'incomplete'; incompleteTurn = true; }
        statusDirty = true;
      }
      if (type === 'session_compact' || type === 'agent_settled') {
        if (type === 'session_compact') queue(activityEntry(event.compactionEntry, manager));
        reconcile();
        if (type === 'agent_settled' && (activeId || toolEntryId)) { clearLive(); reset = true; incompleteTurn = true; }
        state = lateJoin || incompleteTurn ? 'incomplete' : 'ready'; statusDirty = true; emit();
      }
    },
    close() { if (closed) return; closed = true; clearInterval(timer); listeners.clear(); dirty.clear(); removed.clear(); cursors.clear(); pendingCommits = []; liveTools.clear(); liveToolBytes = 0; toolsDirty = false; activeEntry = undefined; toolEntryId = undefined; },
  };
}

export async function listenPrivateSocket(server, path) {
  const parent = await lstat(dirname(path), { bigint: true });
  const uid = BigInt(process.getuid());
  if (!parent.isDirectory() || parent.isSymbolicLink() || parent.uid !== uid || (parent.mode & 0o777n) !== 0o700n) throw new Error('Unsafe socket directory');
  const listen = () => new Promise((resolve, reject) => {
    const failed = error => { server.removeListener('listening', ready); reject(error); };
    const ready = () => { server.removeListener('error', failed); resolve(); };
    server.once('error', failed); server.once('listening', ready); server.listen(path);
  });
  try { await listen(); return; }
  catch (error) {
    if (error.code !== 'EADDRINUSE') throw error;
    const before = await lstat(path, { bigint: true });
    if (!before.isSocket() || before.isSymbolicLink() || before.uid !== uid) throw error;
    const refused = await new Promise(resolve => {
      const socket = createConnection(path);
      const finish = value => { socket.destroy(); resolve(value); };
      socket.once('connect', () => finish(false));
      socket.once('error', failure => finish(failure.code === 'ECONNREFUSED'));
      socket.setTimeout(250, () => finish(false));
    });
    if (!refused) throw error;
    const after = await lstat(path, { bigint: true });
    const directory = await lstat(dirname(path), { bigint: true });
    if (!after.isSocket() || after.uid !== uid || after.dev !== before.dev || after.ino !== before.ino
      || !directory.isDirectory() || directory.isSymbolicLink() || directory.uid !== uid || directory.dev !== parent.dev || directory.ino !== parent.ino || (directory.mode & 0o777n) !== 0o700n) throw error;
    await unlink(path);
    await listen();
  }
}

export async function startActivityServer({ directory, identity, source }) {
  const stat = await lstat(directory);
  if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== process.getuid() || (stat.mode & 0o777) !== 0o700) throw new Error('Unsafe activity directory');
  const path = join(directory, `${identity.pid}.activity.sock`);
  const sockets = new Set();
  const pending = new Set();
  const streams = new Set();
  const pages = new Set();
  let closed = false;
  const server = createServer(socket => {
    if (closed || pending.size >= ACTIVITY_LIMITS.pending) { socket.destroy(); return; }
    sockets.add(socket); pending.add(socket);
    let unlisten;
    let heartbeat;
    const handshakeExpires = performance.now() + ACTIVITY_LIMITS.handshakeMs;
    const handshakeTimer = setTimeout(() => socket.destroy(), ACTIVITY_LIMITS.handshakeMs);
    handshakeTimer.unref?.();
    socket.on('error', () => {});
    socket.on('close', () => { clearTimeout(handshakeTimer); sockets.delete(socket); pending.delete(socket); streams.delete(socket); pages.delete(socket); unlisten?.(); clearInterval(heartbeat); });
    socket.setTimeout(1000, () => socket.destroy());
    let requestBytes = Buffer.alloc(0);
    socket.on('data', function receive(chunk) {
      if (performance.now() >= handshakeExpires) { socket.destroy(); return; }
      requestBytes = Buffer.concat([requestBytes, chunk]);
      if (requestBytes.length > 4096) { socket.destroy(); return; }
      if (!requestBytes.includes(10)) return;
      socket.removeListener('data', receive);
      try {
        const request = JSON.parse(requestBytes.toString('utf8'));
        if (!request || request.instance !== identity.instance || request.pane !== identity.pane || request.pid !== identity.pid || ![identity.session, identity.sessionFile].filter(Boolean).includes(request.session) || !/^[a-f0-9]{32}$/.test(request.challenge) || !['page', 'subscribe'].includes(request.operation)) throw new Error('Invalid activity request');
        const admitted = request.operation === 'page' ? pages : streams;
        const limit = request.operation === 'page' ? ACTIVITY_LIMITS.pages : ACTIVITY_LIMITS.streams;
        if (admitted.size >= limit) { socket.destroy(); return; }
        pending.delete(socket); admitted.add(socket);
        clearTimeout(handshakeTimer);
        const send = frame => {
          const payload = JSON.stringify({ ...frame, ...identity, sessionFile: undefined, session: request.session, challenge: request.challenge, incarnation: source.incarnation });
          if (Buffer.byteLength(payload) > ACTIVITY_LIMITS.frameBytes || socket.writableLength > ACTIVITY_LIMITS.frameBytes) { socket.destroy(); return; }
          socket.write(`${payload}\n`);
        };
        if (request.operation === 'page') {
          send({ kind: 'page', page: source.page(typeof request.cursor === 'string' ? request.cursor : '', request.limit) });
          socket.end(); return;
        }
        socket.setTimeout(0);
        const buffered = [];
        let snapshotting = true;
        unlisten = source.listen(frame => { if (snapshotting) buffered.push(frame); else send(frame); });
        const page = source.page();
        send({ kind: 'snapshot', page, sequence: page.sequence });
        snapshotting = false;
        for (const frame of buffered) if (frame.sequence > page.sequence) send(frame);
        heartbeat = setInterval(() => send({ kind: 'heartbeat', ...source.info() }), 5000);
        heartbeat.unref?.();
      } catch { socket.destroy(); }
    });
  });
  await listenPrivateSocket(server, path);
  server.on('error', () => {});
  try { await chmod(path, 0o600); }
  catch (error) { await new Promise(resolve => server.close(resolve)); throw error; }
  server.unref();
  return async () => {
    if (closed) return;
    closed = true;
    for (const socket of sockets) socket.destroy();
    await new Promise(resolve => server.close(resolve));
    source.close();
  };
}

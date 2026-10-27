import assert from 'node:assert/strict';
import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { syncBuiltinESMExports } from 'node:module';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { crc32, deflateSync } from 'node:zlib';
import net from 'node:net';
import tls from 'node:tls';
import http from 'node:http';
import https from 'node:https';
import { createActivitySource } from '../relay/pi-command-bridge/activity.mjs';

const packageRoot = resolve(process.env.PI_TEST_PACKAGE || '');
const work = resolve(process.env.PI_LIVE_TEST_WORK || '');
assert(process.env.PI_TEST_PACKAGE && process.env.PI_LIVE_TEST_WORK, 'Explicit package and private work directory are required');
assert.equal(process.env.HOME, join(work, 'home'), 'Run with a private HOME');
assert.equal(process.env.PI_CODING_AGENT_DIR, join(work, 'agent'), 'Run with a private agent directory');
const manifest = JSON.parse(await readFile(join(packageRoot, 'package.json'), 'utf8'));
assert.equal(manifest.name, '@earendil-works/pi-coding-agent');
assert.equal(manifest.version, '1.3.0');

let networkAttempts = 0;
const denyNetwork = () => { networkAttempts++; throw new Error('Network is forbidden in the synthetic contract fixture'); };
globalThis.fetch = denyNetwork;
net.Socket.prototype.connect = denyNetwork;
net.connect = denyNetwork;
net.createConnection = denyNetwork;
tls.connect = denyNetwork;
http.request = denyNetwork;
http.get = denyNetwork;
https.request = denyNetwork;
https.get = denyNetwork;
syncBuiltinESMExports();

const { createAgentSessionRuntime, createAgentSessionServices, createAgentSessionFromServices, ModelRuntime, SessionManager, SettingsManager } = await import(pathToFileURL(join(packageRoot, manifest.main)));
const aiRoot = resolve(packageRoot, '../pi-ai');
const aiManifest = JSON.parse(await readFile(join(aiRoot, 'package.json'), 'utf8'));
assert.equal(aiManifest.name, '@earendil-works/pi-ai');
assert.equal(aiManifest.version, '1.3.0');
const { Type, createAssistantMessageEventStream } = await import(pathToFileURL(join(aiRoot, aiManifest.main)));
const cwd = join(work, 'project');
const agentDir = join(work, 'agent');
await mkdir(cwd, { recursive: true, mode: 0o700 });
await mkdir(agentDir, { recursive: true, mode: 0o700 });
const checks = [];
const check = (name, action) => { action(); checks.push(name); };
const errors = [];
const starts = [];
const shutdowns = [];
const boundaries = [];
const treeEvents = [];
const toolEvents = [];
const snapshots = [];
let currentContext;
let providerCalls = 0;
let lateEvents = 0;
let unsubscribeLate;
let partialObserved = false;
let prePersistenceObserved = false;
let compactObserved = false;
let instance = 0;
let activitySource;
let lateToolSource;
const lateToolStates = [];
let providerBehavior = 'normal';
let partialReady = () => {};
const activityFrames = [];
const activityToolSnapshots = [];
const model = {
  id: 'synthetic', name: 'Synthetic contract fixture', provider: 'herdr-live-fixture',
  api: 'herdr-live-fixture', baseUrl: 'http://invalid.invalid', reasoning: false,
  input: ['text', 'image'], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
  contextWindow: 100000, maxTokens: 1024,
};
const usage = () => ({ input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } });
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const pngChunk = (type, data) => {
  const body = Buffer.concat([Buffer.from(type), data]); const size = Buffer.alloc(4); const crc = Buffer.alloc(4);
  size.writeUInt32BE(data.length); crc.writeUInt32BE(crc32(body)); return Buffer.concat([size, body, crc]);
};
const dimensions = Buffer.alloc(13); dimensions.writeUInt32BE(1); dimensions.writeUInt32BE(1, 4); dimensions[8] = 8; dimensions[9] = 6;
const syntheticImage = Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), pngChunk('IHDR', dimensions), pngChunk('IDAT', deflateSync(Buffer.from([0, 255, 255, 255, 255]))), pngChunk('IEND', Buffer.alloc(0))]).toString('base64');
const syntheticStream = (_model, _context, options) => {
  const stream = createAssistantMessageEventStream();
  const call = ++providerCalls;
  const behavior = providerBehavior;
  void (async () => {
    const message = { role: 'assistant', content: [], api: model.api, provider: model.provider, model: model.id, usage: usage(), stopReason: 'pending', timestamp: Date.now() };
    try {
      options?.signal?.throwIfAborted();
      stream.push({ type: 'start', partial: structuredClone(message) });
      message.content.push({ type: 'text', text: '' });
      stream.push({ type: 'text_start', contentIndex: 0, partial: structuredClone(message) });
      for (const delta of ['SYNTHETIC_EARLY_', 'TEXT']) {
        message.content[0].text += delta;
        stream.push({ type: 'text_delta', contentIndex: 0, delta, partial: structuredClone(message) });
        if (behavior === 'abort') await sleep(100);
        else await sleep(5);
        options?.signal?.throwIfAborted();
      }
      if (behavior === 'error') throw new Error('SYNTHETIC_PROVIDER_ERROR');
      stream.push({ type: 'text_end', contentIndex: 0, content: message.content[0].text, partial: structuredClone(message) });
      if (call === 1) {
        for (const id of ['parallel-a', 'parallel-b']) {
          const contentIndex = message.content.length;
          message.content.push({ type: 'toolCall', id, name: 'fixture_parent', arguments: {} });
          stream.push({ type: 'toolcall_start', contentIndex, partial: structuredClone(message) });
          stream.push({ type: 'toolcall_end', contentIndex, toolCall: structuredClone(message.content[contentIndex]), partial: structuredClone(message) });
        }
      }
      message.stopReason = call === 1 ? 'toolUse' : 'stop';
      stream.push({ type: 'done', reason: message.stopReason, message });
      stream.end();
    } catch (error) {
      message.stopReason = options?.signal?.aborted ? 'aborted' : 'error';
      message.errorMessage = String(error.message);
      stream.push({ type: 'error', reason: message.stopReason, error: message });
      stream.end();
    }
  })();
  return stream;
};
const observer = pi => {
  const incarnation = ++instance;
  pi.on('session_start', (event, ctx) => { currentContext = ctx; starts.push({ incarnation, reason: event.reason }); });
  pi.on('session_shutdown', event => { shutdowns.push({ incarnation, reason: event.reason }); unsubscribeLate?.(); });
  pi.on('message_update', (event, ctx) => {
    if (event.message.role !== 'assistant') return;
    const text = event.message.content.filter(block => block.type === 'text').map(block => block.text).join('');
    if (!text) return;
    partialObserved = true;
    assert(!ctx.sessionManager.getBranch().some(entry => entry.type === 'message' && entry.message.role === 'assistant' && entry.message.stopReason === 'pending'));
    if (!unsubscribeLate) unsubscribeLate = pi.on('message_update', () => { lateEvents++; });
  });
  pi.on('message_end', async (event, ctx) => {
    if (event.message.role !== 'assistant') return;
    const leaf = ctx.sessionManager.getLeafId();
    await sleep(5);
    assert.equal(ctx.sessionManager.getLeafId(), leaf);
    prePersistenceObserved = true;
  });
  pi.on('turn_end', (event, ctx) => {
    const assistant = ctx.sessionManager.getEntry(event.messageEntryId);
    assert.equal(assistant?.type, 'message');
    assert.equal(assistant.message.role, 'assistant');
    assert.equal(assistant.message.content[0].text, 'FINAL_VISIBLE_TEXT');
    const results = event.toolResultEntryIds.map(id => ctx.sessionManager.getEntry(id));
    assert(results.every(entry => entry?.type === 'message' && entry.message.role === 'toolResult'));
    assert.deepEqual(new Set(results.map(entry => entry.message.toolCallId)), new Set(event.toolResults.map(result => result.toolCallId)));
    assert(results.every(entry => entry.message.content[0].text === 'FINAL_VISIBLE_TOOL'));
    boundaries.push({ assistantId: assistant.id, toolIds: results.map(entry => entry.id) });
    snapshots.push(ctx.sessionManager.getBranch().map(entry => entry.id));
  });
  for (const type of ['tool_execution_start', 'tool_execution_update', 'tool_execution_end']) {
    pi.on(type, event => { toolEvents.push({ type, id: event.toolCallId, parent: event.parentToolCallId }); });
  }
  pi.on('session_tree', event => treeEvents.push({ old: event.oldLeafId, next: event.newLeafId }));
  pi.on('session_before_compact', event => ({ compaction: { summary: 'Synthetic compaction boundary', firstKeptEntryId: event.preparation.firstKeptEntryId, tokensBefore: event.preparation.tokensBefore } }));
  pi.on('session_compact', () => { compactObserved = true; });
};
const transformer = pi => {
  pi.on('message_end', event => {
    if (event.message.role !== 'assistant') return;
    return { message: { ...event.message, content: event.message.content.map(block => block.type === 'text' ? { type: 'text', text: 'FINAL_VISIBLE_TEXT' } : block) } };
  });
  pi.on('tool_result', event => event.toolName === 'fixture_nested' ? undefined : ({ content: [{ type: 'text', text: 'FINAL_VISIBLE_TOOL' }], details: undefined }));
};
const fixtures = pi => {
  pi.registerProvider(model.provider, { api: model.api, baseUrl: model.baseUrl, apiKey: 'synthetic-not-a-credential', models: [model], streamSimple: syntheticStream });
  pi.registerTool({
    name: 'fixture_nested', label: 'Fixture nested', description: 'Returns a synthetic image', exposure: 'deferred', parameters: Type.Object({}),
    async execute(_id, _args, _signal, onUpdate) {
      const image = { type: 'image', data: syntheticImage, mimeType: 'image/png' };
      onUpdate?.({ content: [{ type: 'text', text: 'SYNTHETIC_NESTED_PARTIAL' }, image], details: undefined });
      await sleep(5);
      return { content: [image], details: undefined };
    },
  });
  pi.registerTool({
    name: 'fixture_parent', label: 'Fixture parent', description: 'Calls the synthetic nested fixture', parameters: Type.Object({}), executionMode: 'parallel',
    async execute(_id, _args, signal, onUpdate, ctx) {
      onUpdate?.({ content: [{ type: 'text', text: 'SYNTHETIC_TOOL_PARTIAL' }], details: { hidden: 'PRIVATE_TOOL_DETAILS' } });
      await ctx.executeTool('fixture_nested', {}, { signal });
      return { content: [{ type: 'text', text: 'SYNTHETIC_TOOL_FINAL' }], details: undefined };
    },
  });
};
const activityAdapter = pi => {
  pi.on('session_start', (_event, ctx) => {
    lateToolSource = undefined;
    activitySource = createActivitySource(ctx.sessionManager, { incomplete: !ctx.isIdle() });
    activitySource.listen(frame => activityFrames.push({ kind: frame.kind, sequence: frame.sequence, entries: frame.entries }));
  });
  for (const type of ['message_start', 'message_update', 'tool_execution_start', 'tool_execution_update', 'tool_execution_end', 'turn_end', 'session_tree', 'session_compact', 'agent_settled']) pi.on(type, (event, ctx) => {
    activitySource.event(type, event, ctx);
    if (type === 'message_update') activitySource.page();
    if (type.startsWith('tool_execution_') && !lateToolSource) lateToolSource = createActivitySource(ctx.sessionManager, { incomplete: true });
    lateToolSource?.event(type, event, ctx);
    if (type.startsWith('tool_execution_')) {
      lateToolStates.push(lateToolSource.page().live_state);
      activityToolSnapshots.push({ type, page: activitySource.page() });
    }
    if (providerBehavior === 'abort' && type === 'message_update' && event.message?.role === 'assistant' && event.message.content.some(block => block.type === 'text' && block.text)) partialReady();
  });
  pi.on('session_shutdown', () => { activitySource.close(); lateToolSource?.close(); });
};
const settingsManager = SettingsManager.inMemory({ compaction: { enabled: false, keepRecentTokens: 1 }, retry: { enabled: false }, cacheWarming: 'off', images: { autoResize: false } });
const resourceLoaderOptions = { noExtensions: true, noSkills: true, noPromptTemplates: true, noThemes: true, noContextFiles: true, systemPrompt: 'Synthetic test only', extensionFactories: [observer, transformer, fixtures, activityAdapter] };
const modelRuntime = await ModelRuntime.create({ authPath: join(agentDir, 'auth.json'), modelsPath: null, allowModelNetwork: false, refreshOnCreate: false });
const sessionManager = SessionManager.create(cwd, join(work, 'sessions'));
const factory = async ({ cwd, agentDir, sessionManager, sessionStartEvent }) => {
  const services = await createAgentSessionServices({ cwd, agentDir, settingsManager, modelRuntime, resourceLoaderOptions });
  assert.deepEqual(services.resourceLoader.getExtensions().errors, []);
  const created = await createAgentSessionFromServices({ services, model, thinkingLevel: 'off', sessionManager, sessionStartEvent, tools: ['fixture_parent', 'fixture_nested'] });
  return { ...created, services, diagnostics: services.diagnostics };
};
let runtime;
let session;
try {
  runtime = await createAgentSessionRuntime(factory, { cwd, agentDir, sessionManager });
  const bind = async next => { session = next; await session.bindExtensions({ mode: 'json', onError: error => errors.push({ event: error.event, error: error.error }) }); };
  runtime.setRebindSession(bind);
  await bind(runtime.session);
  await session.prompt('Synthetic fixture prompt', { expandPromptTemplates: false });
  check('relay activity adapter converges on native committed IDs after always streaming provisional text', () => {
    const page = activitySource.page();
    assert(activityFrames.some(frame => frame.entries.some(entry => entry.id.startsWith('live:') && entry.text.includes('SYNTHETIC_EARLY_'))));
    assert.equal(page.provisional.length, 0);
    assert(boundaries.every(boundary => page.entries.some(entry => entry.id === `pi:${boundary.assistantId}`)));
    assert(page.entries.filter(entry => entry.role === 'assistant').every(entry => entry.text === 'FINAL_VISIBLE_TEXT'));
    assert(activityFrames.length > 0);
  });
  check('public tool start/update/end events preserve parallel and nested progress by default', () => {
    const tools = activityToolSnapshots.flatMap(snapshot => snapshot.page.provisional.flatMap(entry => entry.tools || []));
    assert(tools.some(tool => tool.state === 'running' && tool.output === 'SYNTHETIC_TOOL_PARTIAL'));
    assert(tools.some(tool => tool.parent_id));
    assert(tools.some(tool => tool.state === 'completed'));
    assert(!JSON.stringify(activityToolSnapshots).includes('PRIVATE_TOOL_DETAILS'));
    assert.equal(activitySource.page().provisional.length, 0);
  });
  check('public mixed and image-only tool results retain omission notices without image bytes', () => {
    const nested = activityToolSnapshots.flatMap(snapshot => snapshot.page.provisional.flatMap(entry => entry.tools || [])).filter(tool => tool.parent_id);
    assert(nested.some(tool => tool.state === 'running' && tool.output?.includes('SYNTHETIC_NESTED_PARTIAL') && tool.output.includes('Image tool output omitted')));
    assert(nested.some(tool => tool.state === 'completed' && tool.output === 'Image tool output omitted from this conversation view.'));
    assert(!JSON.stringify(activityToolSnapshots).includes(syntheticImage));
  });
  check('late attachment during public tool execution remains incomplete until persisted reconciliation', () => {
    assert(lateToolStates.length > 0 && lateToolStates.every(state => state === 'incomplete'));
    assert.equal(lateToolSource.page().live_state, 'ready');
  });
  check('public normalized partial events precede persistence', () => assert(partialObserved && prePersistenceObserved));
  check('late event registration receives subsequent updates', () => assert(lateEvents > 0));
  check('turn_end IDs resolve transformed persisted messages and tool results', () => {
    assert.equal(boundaries.length, 2);
    assert.equal(new Set(boundaries.map(boundary => boundary.assistantId)).size, 2);
    assert.equal(boundaries[0].toolIds.length, 2);
  });
  check('ordinary appends preserve earlier branch entries', () => assert(snapshots[0].every(id => snapshots[1].includes(id))));
  check('parallel and nested tool IDs have no independent nested transcript entries', () => {
    const nested = toolEvents.filter(event => event.parent);
    assert(nested.length >= 6);
    assert(new Set(nested.map(event => event.parent)).size === 2);
    const persisted = sessionManager.getBranch().filter(entry => entry.type === 'message' && entry.message.role === 'toolResult');
    assert.equal(persisted.length, 2);
    assert(persisted.every(entry => !nested.some(event => event.id === entry.message.toolCallId)));
  });
  const abandonedLeaf = sessionManager.getLeafId();
  await session.navigateTree(boundaries[0].assistantId, { summarize: false });
  check('branch navigation without another prompt emits session_tree and excludes abandoned leaf', () => {
    assert.equal(treeEvents.length, 1);
    assert.equal(treeEvents[0].old, abandonedLeaf);
    assert.equal(currentContext.sessionManager.getLeafId(), boundaries[0].assistantId);
    assert(!currentContext.sessionManager.getBranch().some(entry => entry.id === abandonedLeaf));
    assert(currentContext.sessionManager.getEntry(abandonedLeaf));
  });
  await session.navigateTree(abandonedLeaf, { summarize: false });
  const beforeCompact = sessionManager.getBranch().map(entry => entry.id);
  await session.compact();
  check('compaction keeps raw branch history and emits lifecycle event', () => {
    assert(compactObserved);
    assert(beforeCompact.every(id => sessionManager.getBranch().some(entry => entry.id === id)));
    assert.equal(sessionManager.getLeafEntry().type, 'compaction');
    assert(sessionManager.buildContextEntries().length < sessionManager.getBranch().length);
  });
  const leafBeforeReload = sessionManager.getLeafId();
  await session.reload();
  check('reload shuts down old observer before starting a fresh incarnation', () => {
    assert.deepEqual(shutdowns, [{ incarnation: 1, reason: 'reload' }]);
    assert.deepEqual(starts, [{ incarnation: 1, reason: 'startup' }, { incarnation: 2, reason: 'reload' }]);
    assert.equal(sessionManager.getLeafId(), leafBeforeReload);
  });
  const originalFile = sessionManager.getSessionFile();
  const originalId = sessionManager.getSessionId();
  await runtime.newSession();
  check('session replacement shuts down old observer and starts a new session identity', () => {
    assert.equal(shutdowns.at(-1).reason, 'new');
    assert.equal(starts.at(-1).reason, 'new');
    assert.notEqual(currentContext.sessionManager.getSessionId(), originalId);
    assert.equal(currentContext.sessionManager.getBranch().filter(entry => entry.type === 'message').length, 0);
  });
  await runtime.switchSession(originalFile);
  check('resume starts a fresh observer on the restored persisted branch', () => {
    assert.equal(shutdowns.at(-1).reason, 'resume');
    assert.equal(starts.at(-1).reason, 'resume');
    assert.equal(currentContext.sessionManager.getSessionId(), originalId);
    assert.equal(currentContext.sessionManager.getLeafId(), leafBeforeReload);
  });
  providerBehavior = 'abort';
  const ready = new Promise(resolve => { partialReady = resolve; });
  const abortedPrompt = session.prompt('Synthetic abort prompt', { expandPromptTemplates: false });
  await ready;
  check('an attached observer receives the persisted user prompt before assistant completion', () => {
    const page = activitySource.page();
    assert.equal(page.provisional.length, 1);
    assert(page.entries.some(entry => entry.role === 'user' && entry.text === 'Synthetic abort prompt'));
  });
  await session.abort();
  await abortedPrompt;
  check('public abort removes the provisional tail and retains its persisted ID with incomplete status', () => {
    const message = currentContext.sessionManager.getBranch().filter(entry => entry.type === 'message' && entry.message.role === 'assistant').at(-1);
    assert.equal(message.message.stopReason, 'aborted');
    const page = activitySource.page();
    assert.equal(page.provisional.length, 0);
    assert.equal(page.live_state, 'incomplete');
    assert(page.entries.some(entry => entry.id === `pi:${message.id}`));
  });
  providerBehavior = 'error';
  await session.prompt('Synthetic error prompt', { expandPromptTemplates: false });
  check('provider error converges without exposing raw error details or a provisional duplicate', () => {
    const message = currentContext.sessionManager.getBranch().filter(entry => entry.type === 'message' && entry.message.role === 'assistant').at(-1);
    assert.equal(message.message.stopReason, 'error');
    const page = activitySource.page();
    assert.equal(page.provisional.length, 0);
    assert.equal(page.live_state, 'incomplete');
    assert(!JSON.stringify(page).includes('SYNTHETIC_PROVIDER_ERROR'));
  });
  providerBehavior = 'normal';
  await session.prompt('Synthetic recovery prompt', { expandPromptTemplates: false });
  check('a subsequent successful turn restores ready state without replaying a prompt', () => assert.equal(activitySource.page().live_state, 'ready'));
  for (const text of ['', 'Synthetic mixed image prompt']) {
    await session.prompt(text, { images: [{ type: 'image', data: syntheticImage, mimeType: 'image/png' }], expandPromptTemplates: false });
    check(`public ${text ? 'mixed' : 'image-only'} user prompt retains an ordered omission marker without image bytes`, () => {
      const native = currentContext.sessionManager.getBranch().filter(entry => entry.type === 'message' && entry.message.role === 'user').at(-1);
      assert(native.message.content.some(block => block.type === 'image'));
      const page = activitySource.page();
      const entry = page.entries.find(entry => entry.id === `pi:${native.id}`);
      assert(entry && entry.role === 'user' && entry.text.includes('Image content omitted'));
      if (text) assert(entry.text.includes(text));
      assert(!JSON.stringify(page).includes(syntheticImage));
    });
  }
  await runtime.dispose();
  check('runtime disposal tears down the last observer once', () => {
    assert.equal(shutdowns.at(-1).reason, 'quit');
    assert.equal(shutdowns.length, starts.length);
  });
  runtime = undefined;
  check('stock extension dispatch reported no errors', () => assert.deepEqual(errors, []));
  check('network guard observed zero attempts', () => assert.equal(networkAttempts, 0));
  const result = { schema: 1, qualification: 'stock-runtime synthetic SDK public API; not TUI or real-provider qualification', version: manifest.version, checks, providerCalls, networkAttempts, sessionReplacementQualified: true, activeTUIQualified: false };
  await writeFile(join(work, 'result.json'), JSON.stringify(result, null, 2) + '\n', { mode: 0o600 });
  console.log(JSON.stringify(result));
} catch (error) {
  await writeFile(join(work, 'failure.json'), JSON.stringify({ schema: 1, checks, extensionErrors: errors, networkAttempts, error: error.message }, null, 2) + '\n', { mode: 0o600 });
  throw error;
} finally {
  await runtime?.dispose();
}

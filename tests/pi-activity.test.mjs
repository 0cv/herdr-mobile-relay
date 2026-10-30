import { test, expect } from 'bun:test';
import { mkdtemp, rm, lstat, mkdir } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { createConnection } from 'node:net';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { createActivitySource, activityEntry, startActivityServer, ACTIVITY_LIMITS } from '../relay/pi-command-bridge/activity.mjs';

function fixture() {
  const entries = new Map();
  let leaf = null;
  const manager = { getLeafId: () => leaf, getEntry: id => entries.get(id), getSessionId: () => 'fixture-session' };
  const add = (id, message, type = 'message') => { const entry = { id, parentId: leaf, type, timestamp: '2026-10-16T00:00:00Z', message }; entries.set(id, entry); leaf = id; return entry; };
  return { manager, add, entries, branch: id => { leaf = id; } };
}
const user = text => ({ role: 'user', content: text });
const assistant = text => ({ role: 'assistant', content: [{ type: 'text', text }] });

test('activity projection exports permitted text/tool fields only and bounds payloads', () => {
  const message = assistant('visible');
  message.content.push({ type: 'thinking', thinking: 'HIDDEN_THINKING' }, { type: 'image', data: 'HIDDEN_IMAGE' }, { type: 'toolCall', id: 'tool', name: 'read', arguments: { path: '/fixture' }, thoughtSignature: 'HIDDEN_SIGNATURE' });
  message.details = { secret: 'HIDDEN_DETAILS' };
  const projected = activityEntry({ type: 'message', id: 'a', message }, null);
  expect(projected.text).toBe('visible');
  expect(JSON.stringify(projected)).not.toContain('HIDDEN_');
  expect(projected.tools[0].input).toContain('/fixture');
  expect(projected.tools[0].truncated).toBeUndefined();
  expect(projected.truncated).toBe(false);
  for (const role of ['system', 'custom', 'toolResult', 'unknown']) expect(activityEntry({ type: 'message', id: 'a', message: { role, content: 'HIDDEN' } })).toBeNull();
  const large = activityEntry({ type: 'message', id: 'b', message: assistant('界'.repeat(100000)) });
  expect(Buffer.byteLength(large.text)).toBeLessThanOrEqual(ACTIVITY_LIMITS.textBytes);
});

test('tool truncation reflects byte, structural and count limits, not ordinary projection', () => {
  const project = (args, output = 'small') => {
    const message = assistant(''); message.content.push({ type: 'toolCall', id: 'call', name: 'read', arguments: args });
    return activityEntry({ type: 'message', id: 'a', message }, { getEntry: () => ({ message: { role: 'toolResult', toolCallId: 'call', content: output } }) }, new Map([['call', 'result']]));
  };
  expect(project({ path: '/fixture' }).tools[0].truncated).toBeUndefined();
  expect(project({ value: 'x'.repeat(2048) }).tools[0].truncated).toBeUndefined();
  for (const value of [{ value: 'x'.repeat(2049) }, Array.from({ length: 33 }, () => 1), { a: { b: { c: { d: 1 } } } }, Object.fromEntries(Array.from({ length: 33 }, (_, i) => [String(i), i]))]) expect(project(value).tools[0].truncated).toBe(true);
  expect(project({}, '界'.repeat(20000)).tools[0].truncated).toBe(true);
  expect(project({}, 'x'.repeat(ACTIVITY_LIMITS.toolBytes)).tools[0].truncated).toBeUndefined();
  expect(project({}, 'x'.repeat(ACTIVITY_LIMITS.toolBytes + 1)).truncated).toBe(true);
});

test('parallel and nested tools stream bounded progress by default', () => {
  const f = fixture();
  const message = assistant('final text');
  message.content.push(...['a', 'b'].map(id => ({ type: 'toolCall', id, name: 'fixture', arguments: { path: '/fixture' } })));
  f.add('assistant', message);
  const source = createActivitySource(f.manager);
  try {
    for (const id of ['a', 'b']) source.event('tool_execution_start', { toolCallId: id, toolName: 'fixture', args: { path: '/fixture' } });
    source.event('tool_execution_start', { toolCallId: 'a/1', parentToolCallId: 'a', toolName: 'nested', args: {} });
    source.event('tool_execution_update', { toolCallId: 'a', toolName: 'fixture', partialResult: { content: [{ type: 'text', text: 'running output' }, { type: 'thinking', thinking: 'PRIVATE_THOUGHT' }], details: 'PRIVATE_DETAILS' } });
    const page = source.page();
    const tools = page.provisional.flatMap(entry => entry.tools || []);
    expect(tools.map(tool => tool.id)).toEqual(['a', 'b', 'a/1']);
    expect(tools[0].state).toBe('running'); expect(tools[0].output).toContain('running output');
    expect(tools[0].output).toContain('Unsupported tool output omitted');
    expect(tools[2].parent_id).toBe('a');
    expect(page.entries.some(entry => entry.id === 'pi:assistant')).toBe(false);
    expect(JSON.stringify(page)).not.toContain('PRIVATE_');
    source.event('tool_execution_end', { toolCallId: 'a', toolName: 'fixture', result: { content: 'early end' }, isError: true });
    expect(source.page().provisional.flatMap(entry => entry.tools || [])[0].state).toBe('failed');
    f.add('result', { role: 'toolResult', toolCallId: 'a', content: 'redacted final' });
    source.event('turn_end', { messageEntryId: 'assistant', toolResultEntryIds: ['result'] });
    const final = source.page();
    expect(final.provisional).toEqual([]);
    expect(final.entries.find(entry => entry.id === 'pi:assistant').tools[0].output).toBe('redacted final');
    expect(final.entries.flatMap(entry => entry.tools || []).some(tool => tool.id === 'a/1')).toBe(false);
  } finally { source.close(); }
});

test('late tool updates stay incomplete until an authoritative turn boundary or fresh message start', () => {
  const f = fixture();
  const source = createActivitySource(f.manager, { incomplete: true });
  try {
    source.event('tool_execution_update', { toolCallId: 'late', toolName: 'fixture', partialResult: { content: 'current progress' } });
    expect(source.page().live_state).toBe('incomplete');
    source.event('tool_execution_end', { toolCallId: 'late', toolName: 'fixture', result: { content: 'finished' }, isError: false });
    expect(source.page().live_state).toBe('incomplete');
    f.add('a', { ...assistant('persisted answer'), stopReason: 'stop' });
    source.event('turn_end', { messageEntryId: 'a', toolResultEntryIds: [] });
    source.event('agent_settled', {});
    expect(source.page().live_state).toBe('ready');
    expect(source.page().provisional).toEqual([]);
  } finally { source.close(); }
});

test('a fresh assistant start recovers late-load status', () => {
  const f = fixture(); const source = createActivitySource(f.manager, { incomplete: true });
  try {
    expect(source.page().live_state).toBe('incomplete');
    source.event('message_start', { message: assistant('') });
    expect(source.page().live_state).toBe('generating');
  } finally { source.close(); }
});

test('partial messages report block-limit omissions without exporting hidden reasoning', () => {
  const f = fixture(); const source = createActivitySource(f.manager);
  try {
    source.event('message_start', { message: assistant('') });
    source.event('message_update', { message: { role: 'assistant', content: [...Array.from({ length: 128 }, () => ({ type: 'thinking', thinking: 'PRIVATE_THOUGHT' })), { type: 'text', text: 'late visible block' }] } });
    let page = source.page();
    expect(page.provisional[0].truncated).toBe(true);
    expect(page.provisional[0].text).toContain('omitted');
    expect(page.live_state).toBe('incomplete');
    expect(JSON.stringify(page)).not.toContain('PRIVATE_THOUGHT');
    source.event('message_update', { message: { role: 'assistant', content: Array.from({ length: 129 }, (_, index) => ({ type: 'text', text: `block${index}` })) } });
    page = source.page();
    expect(page.provisional[0].text).toContain('block127');
    expect(page.provisional[0].text).not.toContain('block128');
    expect(page.provisional[0].truncated).toBe(true);
  } finally { source.close(); }
});

test('image-only and mixed user messages retain safe ordered omission markers in pages and updates', () => {
  const f = fixture();
  const image = { type: 'image', get data() { throw new Error('Image bytes must not be read'); }, get mimeType() { throw new Error('Image metadata must not be read'); } };
  f.add('image', { role: 'user', content: [image] });
  const source = createActivitySource(f.manager); const frames = [];
  source.listen(frame => frames.push(frame));
  try {
    expect(source.page().entries[0].text).toBe('Image content omitted from this conversation view.');
    f.add('mixed', { role: 'user', content: [{ type: 'text', text: 'Look at this' }, image] });
    const page = source.page();
    expect(page.entries.map(entry => entry.id)).toEqual(['pi:image', 'pi:mixed']);
    expect(page.entries[1].text).toContain('Look at this\nImage content omitted');
    expect(frames.at(-1).entries[0].id).toBe('pi:mixed');
    expect(JSON.stringify(page)).not.toContain('mimeType');
  } finally { source.close(); }
});

test.each(['image-only', 'mixed', 'unsupported'])('%s tool output retains safe omission markers in progress, completion and persisted pages', kind => {
  const f = fixture();
  const binary = { type: kind === 'unsupported' ? 'resource' : 'image', get data() { throw new Error('Tool payload must not be read'); }, get mimeType() { throw new Error('Tool metadata must not be read'); } };
  const content = kind === 'mixed' ? [{ type: 'text', text: 'Visible tool text' }, binary] : [binary];
  const notice = kind === 'unsupported' ? 'Unsupported tool output omitted from this conversation view.' : 'Image tool output omitted from this conversation view.';
  const message = assistant(''); message.content.push({ type: 'toolCall', id: 'call', name: 'fixture', arguments: {} });
  f.add('assistant', message);
  const source = createActivitySource(f.manager);
  const frames = []; source.listen(frame => frames.push(frame));
  try {
    source.event('tool_execution_start', { toolCallId: 'call', toolName: 'fixture', args: {} });
    source.event('tool_execution_update', { toolCallId: 'call', toolName: 'fixture', partialResult: { content } });
    const running = source.page().provisional[0].tools[0];
    expect(running.state).toBe('running');
    expect(running.output).toContain(notice);
    expect(running.truncated).toBeUndefined();
    if (kind === 'mixed') expect(running.output).toContain('Visible tool text');
    else expect(running.output).toBe(notice);
    expect(frames.at(-1).entries[0].tools[0].output).toBe(running.output);
    const isError = kind === 'mixed';
    source.event('tool_execution_end', { toolCallId: 'call', toolName: 'fixture', result: { content }, isError });
    expect(source.page().provisional[0].tools[0].state).toBe(isError ? 'failed' : 'completed');
    f.add('result', { role: 'toolResult', toolCallId: 'call', content, isError });
    source.event('turn_end', { messageEntryId: 'assistant', toolResultEntryIds: ['result'] });
    const page = source.page();
    expect(page.provisional).toEqual([]);
    const completed = page.entries.find(entry => entry.id === 'pi:assistant').tools[0];
    expect(completed.output).toBe(running.output);
    expect(completed.state).toBe(isError ? 'failed' : 'completed');
    expect(JSON.stringify(page)).not.toContain('mimeType');
  } finally { source.close(); }
});

test('tool output omission notices remain bounded and distinguish absent output from argument truncation', () => {
  const project = (args, content) => {
    const message = assistant(''); message.content.push({ type: 'toolCall', id: 'call', name: 'fixture', arguments: args });
    return activityEntry({ type: 'message', id: 'assistant', message }, { getEntry: () => ({ message: { role: 'toolResult', toolCallId: 'call', content } }) }, new Map([['call', 'result']])).tools[0];
  };
  const capped = project({}, [{ type: 'text', text: '界'.repeat(ACTIVITY_LIMITS.toolBytes) }, { type: 'image', data: 'PRIVATE_IMAGE' }]);
  expect(Buffer.byteLength(capped.output)).toBeLessThanOrEqual(ACTIVITY_LIMITS.toolBytes);
  expect(capped.output.endsWith('Image tool output omitted from this conversation view.')).toBe(true);
  expect(capped.output).not.toContain('PRIVATE_IMAGE');
  expect(capped.truncated).toBe(true);
  const structural = project({}, [...Array.from({ length: 128 }, () => ({ type: 'text', text: '' })), { type: 'text', text: 'omitted suffix' }]);
  expect(structural.output).toBe('Tool output omitted by relay limits.');
  expect(structural.truncated).toBe(true);
  const noOutput = project({ value: 'x'.repeat(2049) }, []);
  expect(noOutput.output).toBeUndefined();
  expect(noOutput.truncated).toBe(true);
  expect(project({}, []).output).toBeUndefined();
});

test('tool progress bursts coalesce and the provisional tool group remains bounded', async () => {
  const f = fixture(); const source = createActivitySource(f.manager); const frames = [];
  source.listen(frame => frames.push(frame));
  try {
    source.event('tool_execution_start', { toolCallId: 'call', toolName: 'fixture', args: {} });
    for (let index = 0; index < 1000; index++) source.event('tool_execution_update', { toolCallId: 'call', toolName: 'fixture', partialResult: { content: `progress ${index}` } });
    await new Promise(resolve => setTimeout(resolve, 300));
    expect(frames).toHaveLength(1);
    expect(frames[0].entries[0].tools[0].output).toBe('progress 999');
    for (let index = 0; index < 64; index++) source.event('tool_execution_update', { toolCallId: `large${index}`, toolName: 'fixture', args: { payload: 'x'.repeat(2000) }, partialResult: { content: 'y'.repeat(20000) } });
    const page = source.page();
    expect(Buffer.byteLength(JSON.stringify(page))).toBeLessThan(ACTIVITY_LIMITS.frameBytes);
    expect(page.live_state).toBe('incomplete');
    source.event('agent_settled', {});
    expect(source.page().provisional).toEqual([]);
  } finally { source.close(); }
});

test('oversized commit bursts reset the tail and remain recoverable through bounded pages', () => {
  const f = fixture();
  const source = createActivitySource(f.manager);
  const frames = [];
  source.listen(frame => frames.push(frame));
  try {
    for (let index = 0; index < 30; index++) f.add(`record${index}`, assistant('x'.repeat(ACTIVITY_LIMITS.textBytes)));
    source.event('turn_end', { messageEntryId: 'record29', toolResultEntryIds: [] });
    let page = source.page();
    expect(frames[0].kind).toBe('reset');
    expect(Buffer.byteLength(JSON.stringify(frames[0]))).toBeLessThan(ACTIVITY_LIMITS.frameBytes);
    const recovered = [];
    for (;;) {
      expect(Buffer.byteLength(JSON.stringify(page))).toBeLessThan(ACTIVITY_LIMITS.frameBytes);
      recovered.unshift(...page.entries.map(entry => entry.id));
      if (!page.has_more) break;
      page = source.page(page.next_cursor);
    }
    expect(recovered).toEqual(Array.from({ length: 30 }, (_, index) => `pi:record${index}`));
  } finally { source.close(); }
});

test('native branch cursors survive appends, exclude abandoned branches and expire on navigation', () => {
  const f = fixture();
  f.add('u', user('prompt'));
  f.add('old', assistant('abandoned'));
  f.branch('u');
  f.add('new', assistant('current'));
  const source = createActivitySource(f.manager);
  try {
    const page = source.page('', 1);
    expect(page.entries.map(entry => entry.id)).toEqual(['pi:new']);
    f.add('later', assistant('later'));
    const older = source.page(page.next_cursor, 1);
    expect(older.entries.map(entry => entry.id)).toEqual(['pi:u']);
    expect(older.snapshot_id).toBe(page.snapshot_id);
    expect(source.page().source_revision).toBe(page.source_revision);
    f.branch('old');
    source.event('session_tree', {});
    expect(source.page().entries.map(entry => entry.id)).toEqual(['pi:u', 'pi:old']);
    expect(source.page().source_revision).not.toBe(page.source_revision);
    expect(() => source.page(page.next_cursor)).toThrow();
  } finally { source.close(); }
});

test('provisional IDs reconcile to native IDs after transformed persistence, not by repeated text', () => {
  const f = fixture();
  const source = createActivitySource(f.manager);
  const frames = [];
  source.listen(frame => frames.push(frame));
  try {
    f.add('u', user('prompt'));
    source.event('message_start', { message: assistant('') });
    source.event('message_update', { message: assistant('EARLY') });
    const snapshot = source.page();
    expect(snapshot.provisional).toHaveLength(1);
    const provisional = snapshot.provisional[0].id;
    expect(snapshot.entries.map(entry => entry.id)).toEqual(['pi:u']);
    expect(frames[0].entries.some(entry => entry.id === 'pi:u')).toBe(true);
    f.add('a', assistant('REDACTED_FINAL'));
    source.event('turn_end', { messageEntryId: 'a', toolResultEntryIds: [] });
    source.page();
    const batch = frames.at(-1);
    expect(batch.remove).toContain(provisional);
    expect(batch.entries.map(entry => entry.id)).toEqual(['pi:a']);
    expect(batch.entries.at(-1).text).toBe('REDACTED_FINAL');
    f.add('b', assistant('REDACTED_FINAL'));
    source.event('turn_end', { messageEntryId: 'b', toolResultEntryIds: [] });
    source.page();
    expect(frames.at(-1).entries.map(entry => entry.id)).toEqual(['pi:b']);
  } finally { source.close(); }
});

test('captures a user prompt persisted after assistant start without waiting for a model delta', () => {
  const f = fixture();
  const source = createActivitySource(f.manager);
  const frames = [];
  source.listen(frame => frames.push(frame));
  try {
    source.event('message_start', { message: assistant('') });
    f.add('u', user('accepted prompt'));
    source.page();
    expect(frames.at(-1).entries.map(entry => entry.id)).toEqual(['pi:u']);
    const count = frames.length;
    source.page();
    expect(frames).toHaveLength(count);
  } finally { source.close(); }
});

test('another runtime cannot inject provisional text into the observed session', () => {
  const f = fixture();
  const source = createActivitySource(f.manager);
  const other = { sessionManager: { getSessionId: () => 'another-session' } };
  try {
    source.event('message_start', { message: assistant('') });
    source.event('message_update', { message: assistant('OTHER_RUNTIME_PRIVATE') }, other);
    expect(JSON.stringify(source.page())).not.toContain('OTHER_RUNTIME_PRIVATE');
    expect(source.page().live_state).toBe('incomplete');
  } finally { source.close(); }
});

test('a later in-progress message is not removed when an earlier turn is batched', () => {
  const f = fixture();
  const source = createActivitySource(f.manager);
  try {
    source.event('message_start', { message: assistant('') });
    f.add('a', assistant('first'));
    source.event('turn_end', { messageEntryId: 'a', toolResultEntryIds: [] });
    source.event('message_start', { message: assistant('') });
    source.event('message_update', { message: assistant('second partial') });
    const page = source.page();
    expect(page.provisional).toHaveLength(1);
    expect(page.provisional[0].text).toBe('second partial');
    expect(page.entries.at(-1).id).toBe('pi:a');
  } finally { source.close(); }
});

test('default visibility includes partial text after a fresh start and late loading is explicitly incomplete', () => {
  const f = fixture();
  const source = createActivitySource(f.manager, { incomplete: true });
  try {
    source.event('message_update', { message: assistant('PRIVATE_EARLY') });
    expect(source.page().provisional).toEqual([]);
    expect(source.page().live_state).toBe('incomplete');
    source.event('message_start', { message: assistant('') });
    source.event('message_update', { message: assistant('PRIVATE_EARLY') });
    expect(source.page().provisional[0].text).toBe('PRIVATE_EARLY');
    f.add('final', assistant('safe'));
    source.event('turn_end', { messageEntryId: 'final', toolResultEntryIds: [] });
    const page = source.page();
    expect(JSON.stringify(page)).not.toContain('PRIVATE_EARLY');
    expect(page.entries[0].id).toBe('pi:final');
  } finally { source.close(); }
});

test('tool outputs attach using persisted result IDs; nested ephemeral results are not invented', () => {
  const f = fixture();
  const message = assistant('');
  message.content.push({ type: 'toolCall', id: 'call', name: 'fixture', arguments: {} });
  f.add('a', message);
  f.add('r', { role: 'toolResult', toolCallId: 'call', content: [{ type: 'text', text: 'visible result' }], details: 'PRIVATE_DETAILS', nestedCalls: { calls: [{ id: 'call/1', content: 'PRIVATE_NESTED' }] } });
  const source = createActivitySource(f.manager);
  try {
    const page = source.page();
    expect(page.entries).toHaveLength(1);
    expect(page.entries[0].tools[0].output).toBe('visible result');
    expect(JSON.stringify(page)).not.toContain('PRIVATE_');
  } finally { source.close(); }
});

test('late observers update an already-recorded assistant when its tools finish', () => {
  const f = fixture();
  const message = assistant('');
  message.content.push({ type: 'toolCall', id: 'call', name: 'fixture', arguments: {} });
  f.add('a', message);
  const source = createActivitySource(f.manager, { incomplete: true });
  const frames = [];
  source.listen(frame => frames.push(frame));
  try {
    expect(source.page().entries[0].tools[0].output).toBeUndefined();
    f.add('result', { role: 'toolResult', toolCallId: 'call', content: 'completed tool' });
    source.event('turn_end', { messageEntryId: 'a', toolResultEntryIds: ['result'] });
    source.event('agent_settled', {});
    expect(frames.at(-1).entries.find(entry => entry.id === 'pi:a').tools[0].output).toBe('completed tool');
    expect(source.page().live_state).toBe('ready');
  } finally { source.close(); }
});

test.each(['aborted', 'error'])('a %s response removes provisional content and reports incomplete output', stopReason => {
  const f = fixture();
  const source = createActivitySource(f.manager);
  try {
    source.event('message_start', { message: assistant('') });
    source.event('message_update', { message: assistant('provisional') });
    expect(source.page().provisional).toHaveLength(1);
    f.add('a', { ...assistant('persisted'), stopReason });
    source.event('turn_end', { messageEntryId: 'a', toolResultEntryIds: [] });
    source.event('agent_settled', {});
    expect(source.page().provisional).toEqual([]);
    expect(source.page().live_state).toBe('incomplete');
    source.event('message_start', { message: assistant('') });
    f.add('b', { ...assistant('recovered'), stopReason: 'stop' });
    source.event('turn_end', { messageEntryId: 'b', toolResultEntryIds: [] });
    source.event('agent_settled', {});
    expect(source.page().live_state).toBe('ready');
  } finally { source.close(); }
});

test('settling without a persisted boundary clears an orphaned provisional tail and requires resynchronization', () => {
  const f = fixture();
  const source = createActivitySource(f.manager);
  const frames = [];
  source.listen(frame => frames.push(frame));
  try {
    source.event('message_start', { message: assistant('') });
    source.event('message_update', { message: assistant('provisional') });
    expect(source.page().provisional).toHaveLength(1);
    source.event('agent_settled', {});
    expect(source.page().provisional).toEqual([]);
    expect(source.page().live_state).toBe('incomplete');
    expect(frames.at(-1).kind).toBe('reset');
  } finally { source.close(); }
});

test('partial updates coalesce at the source cadence without history traversal', async () => {
  const f = fixture();
  let reads = 0;
  const manager = { ...f.manager, getEntry: id => { reads++; return f.manager.getEntry(id); } };
  const source = createActivitySource(manager);
  const frames = [];
  source.listen(frame => frames.push(frame));
  try {
    source.event('message_start', { message: assistant('') });
    for (let index = 0; index < 1000; index++) source.event('message_update', { message: assistant(`revision ${index}`) });
    expect(reads).toBe(0);
    await new Promise(resolve => setTimeout(resolve, 300));
    expect(frames).toHaveLength(1);
    expect(frames[0].entries[0].text).toBe('revision 999');
  } finally { source.close(); }
});

test.each(['restart', 'crash'])('stock Node socket %s recovers both private endpoints and preserves unrelated paths', async kind => {
  const work = await mkdtemp(join(tmpdir(), `pi-socket-${kind}-`));
  await mkdir(join(work, 'home'), { mode: 0o700 });
  try {
    const child = spawn('node', [fileURLToPath(new URL(`./fixtures/pi-socket-${kind}.mjs`, import.meta.url))], { env: { PATH: process.env.PATH, HOME: join(work, 'home'), PI_SOCKET_TEST_WORK: work, PI_OFFLINE: '1' }, stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = '', stderr = '';
    child.stdout.on('data', data => { stdout += data.toString(); }); child.stderr.on('data', data => { stderr += data.toString(); });
    const code = await new Promise(resolve => { child.on('exit', resolve); child.on('error', error => { stderr += error.message; resolve(-1); }); });
    expect({ code, stderr }).toEqual({ code: 0, stderr: '' });
    const result = JSON.parse(stdout);
    expect(kind === 'restart' ? result.samePidRestarts : result.recoveredReusedPidPaths).toBe(kind === 'restart' ? 6 : 2);
  } finally { await rm(work, { recursive: true, force: true }); }
});

test('stock Node serves eight concurrent history pages alongside active subscriptions with independent bounded admission', async () => {
  const work = await mkdtemp(join(tmpdir(), 'pi-activity-capacity-'));
  await mkdir(join(work, 'home'), { mode: 0o700 });
  try {
    const child = spawn('node', [fileURLToPath(new URL('./fixtures/pi-activity-capacity.mjs', import.meta.url))], { env: { PATH: process.env.PATH, HOME: join(work, 'home'), PI_SOCKET_TEST_WORK: work, PI_OFFLINE: '1' }, stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = '', stderr = '';
    child.stdout.on('data', data => { stdout += data.toString(); }); child.stderr.on('data', data => { stderr += data.toString(); });
    const code = await new Promise(resolve => { child.on('exit', resolve); child.on('error', error => { stderr += error.message; resolve(-1); }); });
    expect({ code, stderr }).toEqual({ code: 0, stderr: '' });
    expect(JSON.parse(stdout)).toMatchObject({ concurrentPagesWithOneStream: 8, concurrentOlderPages: 8, saturatedStreams: 8, concurrentPagesWithFullStreams: 8, simultaneousStreamAndPageHandshakes: 16, pendingLimit: 16, quietHandshakeExpiry: 16, tricklingHandshakeExpiry: 16, independentLimitsEnforced: true, subscriptionContinuity: true, capacityReleased: true });
  } finally { await rm(work, { recursive: true, force: true }); }
});

test('private sibling socket validates identity, snapshots before batches, and cleans up', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'pi-activity-test-'));
  const f = fixture();
  f.add('u', user('fixture'));
  const source = createActivitySource(f.manager);
  const identity = { instance: 'a'.repeat(64), pane: 'pane', session: 'fixture-session', pid: process.pid };
  const close = await startActivityServer({ directory, identity, source });
  const path = join(directory, `${process.pid}.activity.sock`);
  const socket = createConnection(path);
  try {
    const frames = [];
    let pending = '';
    const first = new Promise((resolve, reject) => {
      socket.on('error', reject);
      socket.on('data', chunk => { pending += chunk; let index; while ((index = pending.indexOf('\n')) >= 0) { frames.push(JSON.parse(pending.slice(0, index))); pending = pending.slice(index + 1); resolve(); } });
      socket.on('connect', () => socket.write(JSON.stringify({ ...identity, challenge: 'b'.repeat(32), operation: 'subscribe' }) + '\n'));
    });
    await first;
    expect((await lstat(path)).mode & 0o777).toBe(0o600);
    expect(frames[0].kind).toBe('snapshot');
    source.event('message_start', { message: assistant('') });
    source.event('message_update', { message: assistant('partial') });
    await new Promise(resolve => setTimeout(resolve, 300));
    expect(frames[1].kind).toBe('batch');
    expect(frames[1].sequence).toBeGreaterThan(frames[0].page.sequence);
  } finally {
    if (!socket.closed) {
      const closed = new Promise(resolve => socket.once('close', resolve));
      socket.destroy(); await closed;
    }
    await close(); await close();
    await expect(lstat(path)).rejects.toMatchObject({ code: 'ENOENT' });
    const again = await startActivityServer({ directory, identity, source: createActivitySource(f.manager) });
    await again();
    await expect(lstat(path)).rejects.toMatchObject({ code: 'ENOENT' });
    await rm(directory, { recursive: true, force: true });
  }
});

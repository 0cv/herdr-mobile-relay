import { test, expect } from 'bun:test';
import { activityEntry, createActivitySource, ACTIVITY_LIMITS } from '../relay/pi-command-bridge/activity.mjs';

function fixture() {
  const entries = new Map();
  let leaf = null;
  return {
    manager: { getLeafId: () => leaf, getEntry: id => entries.get(id), getSessionId: () => 'synthetic-session' },
    add(entry) { entries.set(entry.id, { parentId: leaf, timestamp: '2026-10-16T00:00:00Z', ...entry }); leaf = entry.id; },
    branch(id) { leaf = id; },
  };
}
const notice = (id, content = 'Use /orchestrate-answer synthetic-run guidance', display = true) => ({ type: 'custom_message', id, display, content, customType: 'any-extension', details: 'PRIVATE_DETAILS' });

test('extension notice projection requires explicit display true and never exports metadata', () => {
  for (const display of [false, undefined, 'true', 1]) expect(activityEntry({ ...notice('hidden', 'PRIVATE_HIDDEN'), display })).toBeNull();
  const entry = activityEntry(notice('visible', [{ type: 'text', text: 'First' }, { type: 'image', data: 'PRIVATE_IMAGE' }, { type: 'thinking', thinking: 'PRIVATE_THINKING' }, { type: 'toolCall', name: 'PRIVATE_TOOL' }, { type: 'text', text: 'Second' }]));
  expect(entry).toMatchObject({ id: 'pi:visible', kind: 'notice', role: 'assistant', text: 'First\nSecond' });
  expect(JSON.stringify(entry)).not.toContain('PRIVATE_');
  expect(entry.tools).toBeUndefined();
  const large = activityEntry(notice('large', '界'.repeat(100000)));
  expect(Buffer.byteLength(large.text)).toBeLessThanOrEqual(ACTIVITY_LIMITS.textBytes);
  expect(large.truncated).toBe(true);
  expect(activityEntry(notice('image', [{ type: 'image', data: 'PRIVATE_IMAGE' }])).text).toBe('Extension message has no displayable text.');
});

test('notice projection never reads hidden content, details, or non-text payloads', () => {
  const privateValue = () => { throw new Error('Private extension data must not be read'); };
  const hidden = { type: 'custom_message', id: 'hidden', display: false, get content() { return privateValue(); }, get details() { return privateValue(); } };
  expect(activityEntry(hidden)).toBeNull();
  const visible = { type: 'custom_message', id: 'visible', display: true, content: [{ type: 'text', text: 'Visible notice' }, { type: 'image', get data() { return privateValue(); } }], get details() { return privateValue(); } };
  expect(activityEntry(visible).text).toBe('Visible notice');
  expect(activityEntry(notice('empty', '\u001b[31m\u0000   ')).text).toBe('Extension message has no displayable text.');
});

test('extension notices appended without an agent turn appear in live batches and preserve paged order', async () => {
  const f = fixture();
  f.add({ type: 'message', id: 'answer', message: { role: 'assistant', content: 'Preceding answer' } });
  const source = createActivitySource(f.manager);
  const frames = [];
  source.listen(frame => frames.push(frame));
  try {
    f.add(notice('hidden', 'PRIVATE_HIDDEN', false));
    f.add(notice('visible'));
    await new Promise(resolve => setTimeout(resolve, 300));
    expect(frames).toHaveLength(1);
    expect(frames[0].entries.map(entry => entry.id)).toEqual(['pi:visible']);
    expect(frames[0].entries[0].kind).toBe('notice');
    expect(frames[0].capabilities.active_branch).toBe(true);
    await new Promise(resolve => setTimeout(resolve, 300));
    expect(frames).toHaveLength(1);
    const page = source.page('', 1);
    expect(page.entries.map(entry => entry.id)).toEqual(['pi:visible']);
    f.add(notice('later', 'Later notice'));
    const older = source.page(page.next_cursor, 1);
    expect(older.entries.map(entry => entry.id)).toEqual(['pi:answer']);
    expect(older.snapshot_id).toBe(page.snapshot_id);
    expect(JSON.stringify(frames)).not.toContain('PRIVATE_');
    f.branch('answer');
    f.add(notice('new-branch', 'New branch notice'));
    source.event('session_tree', {});
    expect(source.page().entries.map(entry => entry.id)).toEqual(['pi:answer', 'pi:new-branch']);
    expect(() => source.page(page.next_cursor)).toThrow();
  } finally { source.close(); }
});

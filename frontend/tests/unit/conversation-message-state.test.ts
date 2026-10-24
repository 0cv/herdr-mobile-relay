import { expect, it } from 'vitest';
import { ConversationMessageStateStore } from '$lib/conversation-message-state';

it('retains UI state while a loaded message leaves the mounted window', () => {
  const store = new ConversationMessageStateStore();
  store.setScope('target:source');
  const state = store.get('message');
  state.tableOffsets[0] = 180; state.openTools.add('tool:0');
  store.retain(new Set(['message', 'other']));
  expect(store.get('message')).toBe(state);
  expect(store.get('message').tableOffsets).toEqual([180]);
  expect(store.get('message').openTools.has('tool:0')).toBe(true);
});

it('drops state on source/target changes and cache eviction', () => {
  const store = new ConversationMessageStateStore();
  store.setScope('target:source'); store.get('message').openTools.add('tool');
  store.setScope('other:source'); expect(store.get('message').openTools.size).toBe(0);
  store.get('message').tableOffsets.push(50); store.retain(new Set());
  expect(store.get('message').tableOffsets).toEqual([]);
  store.get('live:tool').openTools.add('tool'); store.retainLive(new Set());
  expect(store.get('live:tool').openTools.size).toBe(0);
  store.clear(); expect(store.get('message').openTools.size).toBe(0);
});

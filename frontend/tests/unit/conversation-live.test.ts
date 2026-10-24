import { describe, expect, it, vi, afterEach } from 'vitest';
import { ConversationLiveReader } from '$lib/conversation-live';
import { conversationEntries } from '$lib/conversation';
import type { Agent, ConversationBatch, ConversationPage } from '$lib/types';

const agent = { relay_id: 'relay', raw_pane_id: 'pane', pane_id: 'relay:pane', server_session_id: 'primary', terminal_id: 'terminal', generation: 1, agent_session_id: 'session', agent: 'pi', cwd: '/fixture' } as Agent;
const capabilities = { source: 'pi_events' as const, active_branch: true };
const page: ConversationPage = { available: true, reason: '', entries: [], hasMore: false, total: null, state: 'ready', mode: 'snapshot', subscriptionId: 'sub-1', sourceRevision: 'source:1', sequence: 0, capabilities };
const entry = (id: string, text = id) => ({ id, role: 'assistant' as const, timestamp: '', text });
function harness() {
  let receive: (batch: ConversationBatch) => void = () => {};
  const request = vi.fn((_agent: Agent, _revision: string, _after: number, signal: AbortSignal) => new Promise<ConversationBatch>((resolve, reject) => {
    receive = resolve;
    signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
  }));
  const apply = vi.fn(() => true);
  const onState = vi.fn();
  const reset = vi.fn();
  const unsubscribe = vi.fn();
  const reader = new ConversationLiveReader({ request, apply, onState, reset, unsubscribe });
  const batch = async (overrides: Partial<ConversationBatch> = {}) => {
    receive({ subscriptionId: 'sub-1', kind: 'batch', sourceRevision: 'source:1', sequence: 1, entries: [], remove: [], state: 'generating', capabilities, ...overrides });
    await Promise.resolve();
    await Promise.resolve();
  };
  return { reader, batch, apply, request, onState, reset, unsubscribe };
}
afterEach(() => vi.useRealTimers());

describe('conversation live reducer', () => {
  it('does not replace a compact answer with a compaction boundary', () => {
    const answer = entry('pi:answer', 'Final answer');
    expect(conversationEntries([answer, { ...entry('pi:boundary', 'Context compacted'), kind: 'compaction' }])).toEqual([answer]);
  });

  it('updates only the provisional tail and maps final IDs without text deduplication', async () => {
    const h = harness();
    h.reader.observe(agent, page);
    await h.batch({ entries: [entry('live:1', 'same')] });
    expect(h.apply).toHaveBeenLastCalledWith('source:1', [], 1);
    expect(h.onState.mock.calls.at(-1)?.[1]).toEqual([entry('live:1', 'same')]);
    await h.batch({ sequence: 2, entries: [entry('pi:final', 'same')], remove: ['live:1'], state: 'ready' });
    expect(h.apply).toHaveBeenLastCalledWith('source:1', [entry('pi:final', 'same')], 2);
    expect(h.onState.mock.calls.at(-1)?.[1]).toEqual([]);
    h.reader.stop();
  });

  it('rebinds a replacement subscription even when source and target are unchanged', async () => {
    const h = harness();
    h.reader.observe(agent, page);
    h.reader.observe(agent, { ...page, subscriptionId: 'sub-2' });
    expect(h.request).toHaveBeenCalledTimes(2);
    await h.batch({ subscriptionId: 'sub-2', entries: [entry('pi:current')] });
    expect(h.apply).toHaveBeenLastCalledWith('source:1', [entry('pi:current')], 1);
    h.reader.stop();
    expect(h.unsubscribe).toHaveBeenLastCalledWith(agent, 'sub-2');
  });

  it('rebases to a newer same-subscription snapshot without retaining its old provisional tail', async () => {
    const h = harness();
    h.reader.observe(agent, page);
    await h.batch({ entries: [entry('live:old')] });
    h.reader.observe(agent, { ...page, sequence: 2, provisional: [] });
    expect(h.request.mock.calls.at(-1)?.[2]).toBe(2);
    expect(h.onState.mock.calls.at(-1)?.[1]).toEqual([]);
    expect(h.unsubscribe).not.toHaveBeenCalled();
    await h.batch({ sequence: 3, entries: [entry('live:new')] });
    expect(h.onState.mock.calls.at(-1)?.[1]).toEqual([entry('live:new')]);
    h.reader.stop();
  });

  it('does not replace a newer provisional tail with an older snapshot', async () => {
    const h = harness();
    h.reader.observe(agent, page);
    await h.batch({ entries: [entry('live:current')] });
    h.reader.observe(agent, { ...page, provisional: [entry('live:stale')] });
    expect(h.request).toHaveBeenCalledTimes(2);
    expect(h.onState.mock.calls.at(-1)?.[1]).toEqual([entry('live:current')]);
    h.reader.stop();
  });

  it('preserves a snapshot provisional tail for late subscribers', () => {
    const h = harness();
    h.reader.observe(agent, { ...page, provisional: [entry('live:existing')] });
    expect(h.onState).toHaveBeenCalledWith('synchronizing', [entry('live:existing')], capabilities);
    h.reader.stop();
  });

  it.each([{ sequence: 3 }, { sequence: 0 }, { sourceRevision: 'replacement' }, { kind: 'reset' as const }])('resynchronizes instead of applying a stale or missing batch: %o', async overrides => {
    vi.useFakeTimers();
    const h = harness();
    h.reader.observe(agent, page);
    await h.batch(overrides);
    expect(h.apply).not.toHaveBeenCalled();
    expect(h.unsubscribe).toHaveBeenCalledWith(agent, 'sub-1');
    await vi.advanceTimersByTimeAsync(1000);
    expect(h.reset).toHaveBeenCalledOnce();
    h.reader.stop();
  });

  it('stops work on pause and ignores results from an old identity', async () => {
    const h = harness();
    h.reader.observe(agent, page);
    h.reader.pause();
    await h.batch({ entries: [entry('pi:stale')] });
    expect(h.apply).not.toHaveBeenCalled();
    expect(h.unsubscribe).toHaveBeenCalledOnce();
    h.reader.observe(agent, page);
    expect(h.request).toHaveBeenCalledOnce();
    h.reader.resume();
    h.reader.observe({ ...agent, generation: 2 }, page);
    expect(h.request).toHaveBeenCalledTimes(2);
    h.reader.stop();
  });

  it('resynchronizes when the provisional buffer exceeds its entry bound', async () => {
    const h = harness();
    h.reader.observe(agent, page);
    await h.batch({ entries: Array.from({ length: 17 }, (_, index) => entry(`live:${index}`)) });
    expect(h.onState.mock.calls.at(-1)?.[0]).toBe('incomplete');
    expect(h.reader.observing).toBe(false);
    h.reader.stop();
  });
});

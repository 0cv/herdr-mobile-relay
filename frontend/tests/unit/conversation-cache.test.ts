import { afterEach, describe, expect, it } from 'vitest';
import {
  clearConversationPreviews,
  clearConversationPreviewsForRelay,
  configureConversationCache,
  conversationPreviewCacheSize,
  getConversationPreview,
  putConversationPreview,
  resetConversationCacheLimits,
} from '$lib/conversation-cache';
import type { ConversationEntry, OmoTodoState } from '$lib/types';

function entry(id: string, role: 'user' | 'assistant', text = id, tools?: ConversationEntry['tools']): ConversationEntry {
  return {
    id,
    role,
    text,
    timestamp: '2026-01-01T00:00:00Z',
    ...(tools ? { tools } : {}),
  };
}

function preview(identity: string, relayId = 'relay-1', entries: ConversationEntry[] = [entry('u1', 'user', 'question'), entry('a1', 'assistant', 'answer')]) {
  return {
    identity,
    relayId,
    savedAt: 100,
    sourceRevision: 'revision-1',
    historical: false,
    entries,
  };
}

const plan: OmoTodoState = {
  available: true,
  phases: [{ name: 'phase', tasks: [{ content: 'task', status: 'pending' }] }],
  truncated: false,
};

afterEach(() => {
  resetConversationCacheLimits();
  clearConversationPreviews();
});

describe('conversation preview cache', () => {
  it('returns the stored entries, tools, diagnostics, and plans', () => {
    const tools = [{ id: 'tool-1', name: 'Bash', input: 'ls', output: 'ok' }];
    const diagnostics = { oversized_records: 1, corrupt_records: 2, source_truncated: false };
    putConversationPreview({
      ...preview('identity-1', 'relay-1', [entry('u1', 'user', 'question'), entry('a1', 'assistant', 'answer', tools)]),
      diagnostics,
      plan,
    }, 100);

    const stored = getConversationPreview('identity-1', 101)!;
    expect(stored.savedAt).toBe(100);
    expect(stored.entries.map(({ id, text }) => [id, text])).toEqual([['u1', 'question'], ['a1', 'answer']]);
    expect(stored.entries[1].tools).toEqual(tools);
    expect(stored.diagnostics).toEqual(diagnostics);
    expect(stored.plan).toEqual(plan);
    expect(getConversationPreview('identity-1', 102)).toEqual(stored);
  });

  it('omits the plan when none was stored', () => {
    for (const [index, value] of [undefined, null as unknown as OmoTodoState].entries()) {
      const identity = `no-plan-${index}`;
      expect(putConversationPreview({ ...preview(identity), plan: value }, 100)).toBe(true);
      expect(Object.hasOwn(getConversationPreview(identity, 101)!, 'plan')).toBe(false);
    }
  });

  it('expires from the successful save time and clears by relay', () => {
    putConversationPreview(preview('identity-1', 'relay-1'), 100);
    putConversationPreview(preview('identity-2', 'relay-2'), 100);
    expect(getConversationPreview('identity-1', 109)).not.toBeNull();

    configureConversationCache({ ttlMs: 10 });
    // configureConversationCache deliberately resets values; put fresh values
    // to exercise the TTL boundary without relying on wall-clock time.
    putConversationPreview(preview('identity-1', 'relay-1'), 100);
    putConversationPreview(preview('identity-2', 'relay-2'), 100);
    expect(getConversationPreview('identity-1', 109)).not.toBeNull();
    expect(getConversationPreview('identity-1', 110)).toBeNull();

    putConversationPreview(preview('identity-1', 'relay-1'), 100);
    putConversationPreview(preview('identity-2', 'relay-2'), 100);
    clearConversationPreviewsForRelay('relay-1');
    expect(getConversationPreview('identity-1', 101)).toBeNull();
    expect(getConversationPreview('identity-2', 101)).not.toBeNull();
  });

  it('evicts least recently used identities and enforces aggregate bytes', () => {
    configureConversationCache({ maxIdentities: 2, totalBytes: 1_500 });
    expect(putConversationPreview(preview('identity-1'), 1)).toBe(true);
    expect(putConversationPreview(preview('identity-2'), 2)).toBe(true);
    expect(getConversationPreview('identity-1', 3)).not.toBeNull();
    expect(putConversationPreview(preview('identity-3'), 4)).toBe(true);
    expect(getConversationPreview('identity-1', 5)).not.toBeNull();
    expect(getConversationPreview('identity-2', 5)).toBeNull();
    expect(getConversationPreview('identity-3', 5)).not.toBeNull();

    configureConversationCache({ maxIdentities: 6, totalBytes: 1 });
    expect(putConversationPreview(preview('too-large'), 10)).toBe(false);
    expect(conversationPreviewCacheSize().identities).toBe(0);
  });

  it('keeps the newest page without reading or cloning older entries', () => {
    const older = entry('old', 'user');
    Object.defineProperty(older, 'text', { get() { throw new Error('older entry read'); } });
    const entries = [older, ...Array.from({ length: 23 }, (_, index) => entry(`a${index}`, 'assistant')),
      entry('u1', 'user'), entry('a1', 'assistant')];
    expect(putConversationPreview(preview('identity-1', 'relay-1', entries), 100)).toBe(true);
    expect(getConversationPreview('identity-1', 101)?.entries).toEqual(entries.slice(-24));
  });

  it('replaces an existing preview with the newest page of a long assistant turn and keeps short fragments', () => {
    const entries = [entry('u1', 'user'), ...Array.from({ length: 24 }, (_, index) => entry(`a${index}`, 'assistant'))];
    expect(putConversationPreview(preview('long-turn'), 99)).toBe(true);
    expect(putConversationPreview(preview('long-turn', 'relay-1', entries), 100)).toBe(true);
    const saved = getConversationPreview('long-turn', 101)!;
    expect(saved.entries).toEqual(entries.slice(-24));
    expect(saved.savedAt).toBe(100);
    expect(putConversationPreview(preview('fragment', 'relay-1', [entry('a1', 'assistant')]), 100)).toBe(true);
    expect(getConversationPreview('fragment', 101)?.entries.map(({ id }) => id)).toEqual(['a1']);
  });

  it('keeps whole newest exchanges within the byte budget and skips an oversized newest exchange', () => {
    configureConversationCache({ itemBytes: 1_500 });
    expect(putConversationPreview(preview('identity-1', 'relay-1', [
      entry('u0', 'user', 'x'.repeat(1_000)),
      entry('a0', 'assistant'),
      entry('u1', 'user'),
      entry('a1', 'assistant'),
    ]), 100)).toBe(true);
    expect(getConversationPreview('identity-1', 101)?.entries.map(({ id }) => id)).toEqual(['u1', 'a1']);
    expect(putConversationPreview(preview('identity-2', 'relay-1', [
      entry('u1', 'user', 'x'.repeat(1_000)),
      entry('a1', 'assistant', 'answer'),
    ]), 100)).toBe(false);
    expect(getConversationPreview('identity-2', 101)).toBeNull();
  });
});

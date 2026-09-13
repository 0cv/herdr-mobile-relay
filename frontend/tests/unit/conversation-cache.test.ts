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
  it('returns cloned entries, tools, diagnostics, and plans', () => {
    const tools = [{ id: 'tool-1', name: 'Bash', input: 'ls', output: 'ok' }];
    const diagnostics = { oversized_records: 1, corrupt_records: 2, source_truncated: false };
    putConversationPreview({
      ...preview('identity-1', 'relay-1', [entry('u1', 'user', 'question'), entry('a1', 'assistant', 'answer', tools)]),
      diagnostics,
      plan,
    }, 100);

    const first = getConversationPreview('identity-1', 101)!;
    first.entries[1].text = 'mutated';
    first.entries[1].tools![0].output = 'mutated';
    first.diagnostics!.corrupt_records = 99;
    first.plan!.phases[0].tasks[0].content = 'mutated';

    const second = getConversationPreview('identity-1', 102)!;
    expect(second.entries[1].text).toBe('answer');
    expect(second.entries[1].tools![0].output).toBe('ok');
    expect(second.diagnostics?.corrupt_records).toBe(2);
    expect(second.plan?.phases[0].tasks[0].content).toBe('task');
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

  it('keeps whole newest exchanges and skips an oversized newest exchange', () => {
    configureConversationCache({ maxEntries: 4, itemBytes: 1_500 });
    const accepted = putConversationPreview(preview('identity-1', 'relay-1', [
      entry('u0', 'user', 'old question'),
      entry('a0', 'assistant', 'old answer'),
      entry('u1', 'user', 'new question'),
      entry('a1', 'assistant', 'new answer'),
    ]), 100);
    expect(accepted).toBe(true);
    expect(getConversationPreview('identity-1', 101)?.entries.map(({ id }) => id)).toEqual(['u0', 'a0', 'u1', 'a1']);

    configureConversationCache({ maxEntries: 2, itemBytes: 1_500 });
    expect(putConversationPreview(preview('identity-2', 'relay-1', [
      entry('u0', 'user', 'old question'),
      entry('a0', 'assistant', 'old answer'),
      entry('u1', 'user', 'new question'),
      entry('a1', 'assistant', 'new answer'),
    ]), 100)).toBe(true);
    expect(getConversationPreview('identity-2', 101)?.entries.map(({ id }) => id)).toEqual(['u1', 'a1']);

    expect(putConversationPreview(preview('identity-3', 'relay-1', [
      entry('u1', 'user', 'x'.repeat(1_000)),
      entry('a1', 'assistant', 'answer'),
    ]), 100)).toBe(false);
    expect(getConversationPreview('identity-3', 101)).toBeNull();
  });
});

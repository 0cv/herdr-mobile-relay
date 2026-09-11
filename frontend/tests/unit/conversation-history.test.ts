import { describe, expect, it, vi } from 'vitest';
import {
  analyzeConversationBatch,
  conversationIdentity,
  ConversationHistoryController,
  HISTORY_WIRE_PAGE_SIZE,
} from '$lib/conversation-history';
import type { Agent, ConversationEntry, ConversationPage } from '$lib/types';

function agent(rawPaneId = 'pane-1'): Agent {
  return {
    relay_id: 'relay-1',
    relay_label: 'Relay',
    raw_pane_id: rawPaneId,
    pane_id: `relay-1::${rawPaneId}`,
    server_session_id: 'server-1',
    terminal_id: `terminal-${rawPaneId}`,
    generation: 1,
    agent_session_id: 'session-1',
    agent: ' Claude_Code ',
    cwd: '/workspace/project',
    project: 'Project',
    status: 'working',
  };
}

function entry(id: string, role: 'user' | 'assistant', text = id, tools?: ConversationEntry['tools']): ConversationEntry {
  return {
    id,
    role,
    text,
    timestamp: `2026-01-01T00:00:${id.length.toString().padStart(2, '0')}Z`,
    ...(tools ? { tools } : {}),
  };
}

function page(overrides: Partial<ConversationPage> = {}): ConversationPage {
  return {
    available: true,
    reason: '',
    entries: [],
    hasMore: false,
    total: null,
    state: 'ready',
    mode: 'recent',
    ...overrides,
  };
}

describe('conversation history identity and batch analysis', () => {
  it('uses exact target identity while ignoring presentation metadata', () => {
    const first = agent();
    const changed = { ...first, status: 'waiting', project: 'Renamed project', relay_label: 'New label' };
    expect(conversationIdentity(first)).toBe(conversationIdentity(changed));
    expect(conversationIdentity({ ...first, raw_pane_id: 'pane-2' })).not.toBe(conversationIdentity(first));
    expect(conversationIdentity({ ...first, agent_session_id: '' })).toBeNull();
  });

  it('stages a leading fragment but still reports complete exchanges', () => {
    const analysis = analyzeConversationBatch([
      entry('orphan', 'assistant', 'context answer'),
      entry('u1', 'user', 'first question'),
      entry('a1', 'assistant', 'first answer'),
      entry('u2', 'user', 'unanswered question'),
    ], true);
    expect(analysis.pendingPrefix.map(({ id }) => id)).toEqual(['orphan']);
    expect(analysis.needsLeadingContext).toBe(true);
    expect(analysis.completedOlderExchanges).toBe(1);
    expect(analysis.hasUsableLatestExchange).toBe(true);
  });
});

describe('ConversationHistoryController', () => {
  it('serializes initial context loading and requests the wire maximum', async () => {
    const calls: { cursor?: string; limit?: number }[] = [];
    const request = vi.fn(async (_target: Agent, input: { cursor?: string; limit?: number }) => {
      calls.push(input);
      if (calls.length === 1) {
        return page({
          entries: [entry('answer-only', 'assistant', 'latest answer')],
          nextCursor: 'context-cursor',
          hasMore: true,
          sourceRevision: 'revision-1',
        });
      }
      return page({
        entries: [entry('question', 'user', 'prompt')],
        sourceRevision: 'revision-1',
      });
    });
    const controller = new ConversationHistoryController(agent(), {
      request,
      yieldToBrowser: async () => {},
    });
    controller.start();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
    expect(calls.map(({ cursor, limit }) => [cursor, limit])).toEqual([
      [undefined, HISTORY_WIRE_PAGE_SIZE],
      ['context-cursor', HISTORY_WIRE_PAGE_SIZE],
    ]);
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['question', 'answer-only']);
    expect(controller.state.beginningReached).toBe(true);
  });

  it('starts each older demand from current semantic progress and crosses tool-only pages', async () => {
    const requests: { cursor?: string }[] = [];
    const olderExchanges = Array.from({ length: 12 }, (_, index) => [
      entry(`older-user-${index}`, 'user', `older question ${index}`),
      entry(`older-answer-${index}`, 'assistant', `older answer ${index}`),
    ]).flat();
    const toolOnlyPage = Array.from({ length: 200 }, (_, index) => entry(
      `tool-${index}`,
      'assistant',
      '',
      [{ name: 'Read', input: `file-${index}` }],
    ));
    const request = vi.fn(async (_target: Agent, input: { cursor?: string }) => {
      requests.push(input);
      if (requests.length === 1) return page({
        entries: [entry('latest-user', 'user', 'latest question'), entry('latest-answer', 'assistant', 'latest answer')],
        nextCursor: 'older-1',
        hasMore: true,
        sourceRevision: 'revision-1',
      });
      if (requests.length === 2) return page({ entries: olderExchanges, nextCursor: 'older-2', hasMore: true, sourceRevision: 'revision-1' });
      if (requests.length === 3) return page({ entries: toolOnlyPage, nextCursor: 'older-3', hasMore: true, sourceRevision: 'revision-1' });
      return page({
        entries: [entry('earlier-user', 'user', 'earlier question'), entry('earlier-answer', 'assistant', 'earlier answer')],
        sourceRevision: 'revision-1',
      });
    });
    const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {} });
    controller.start();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(1));

    controller.demandOlder();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
    controller.demandOlder();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(4));

    expect(requests.map(({ cursor }) => cursor)).toEqual([undefined, 'older-1', 'older-2', 'older-3']);
    expect(controller.state.entries.map(({ id }) => id)).toContain('earlier-user');
    expect(controller.state.requestPhase).toBe('idle');
    expect(controller.state.contextSearching).toBe(false);
  });

  it('does not crawl a leading older fragment after a usable latest exchange', async () => {
    const request = vi.fn(async () => page({
      entries: [
        entry('orphan', 'assistant', 'unrelated older activity'),
        entry('latest-user', 'user', 'latest question'),
        entry('latest-answer', 'assistant', 'latest answer'),
      ],
      nextCursor: 'older-cursor',
      hasMore: true,
      sourceRevision: 'revision-1',
    }));
    const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {} });
    controller.start();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(1));
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(controller.state.pendingPrefix.map(({ id }) => id)).toEqual(['orphan']);
    expect(controller.state.contextSearching).toBe(false);
    expect(controller.state.requestPhase).toBe('idle');
  });

  it('keeps searching when the newest window contains only tools and assistant activity', async () => {
    let calls = 0;
    const request = vi.fn(async () => {
      calls++;
      if (calls === 1) return page({
        entries: [
          entry('tool-only', 'assistant', '', [{ name: 'Read', input: 'file.txt' }]),
          entry('assistant-activity', 'assistant', 'still working'),
        ],
        nextCursor: 'prompt-cursor',
        hasMore: true,
        sourceRevision: 'revision-1',
      });
      return page({
        entries: [entry('actual-user', 'user', 'actual prompt'), entry('actual-answer', 'assistant', 'actual answer')],
        sourceRevision: 'revision-1',
      });
    });
    const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {} });
    controller.start();
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(request).toHaveBeenCalledTimes(2);
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['actual-user', 'actual-answer', 'tool-only', 'assistant-activity']);
  });

  it('promotes a warm preview when the fresh page has a leading fragment and a usable exchange', async () => {
    const current = agent();
    const request = vi.fn(async () => page({
      entries: [
        entry('fresh-orphan', 'assistant', 'older fragment'),
        entry('fresh-user', 'user', 'fresh question'),
        entry('fresh-answer', 'assistant', 'fresh answer'),
      ],
      nextCursor: 'fresh-older',
      hasMore: true,
      sourceRevision: 'revision-2',
    }));
    const controller = new ConversationHistoryController(current, {
      request,
      getPreview: () => ({
        identity: conversationIdentity(current)!,
        relayId: current.relay_id,
        savedAt: 1,
        sourceRevision: 'revision-1',
        historical: false,
        entries: [entry('cached-user', 'user', 'cached question'), entry('cached-answer', 'assistant', 'cached answer')],
      }),
      yieldToBrowser: async () => {},
    });
    controller.start();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(1));
    await vi.waitFor(() => expect(controller.state.preview).toBe(false));
    expect(controller.state.authoritative).toBe(true);
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['fresh-orphan', 'fresh-user', 'fresh-answer']);
  });

  it('keeps a warm preview separate until fresh context is usable', async () => {
    let resolve!: (value: ConversationPage) => void;
    const request = vi.fn(() => new Promise<ConversationPage>((complete) => { resolve = complete; }));
    const cached = [entry('cached-user', 'user', 'saved question'), entry('cached-answer', 'assistant', 'saved answer')];
    const controller = new ConversationHistoryController(agent(), {
      request,
      getPreview: () => ({
        identity: conversationIdentity(agent())!,
        relayId: 'relay-1',
        savedAt: Date.now(),
        sourceRevision: 'old-revision',
        historical: false,
        entries: cached,
      }),
      yieldToBrowser: async () => {},
    });
    controller.start();
    expect(controller.state.preview).toBe(true);
    expect(controller.state.authoritative).toBe(false);
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['cached-user', 'cached-answer']);

    resolve(page({
      entries: [entry('fresh-answer', 'assistant', 'fresh answer')],
      nextCursor: 'fresh-context',
      hasMore: true,
      sourceRevision: 'new-revision',
    }));
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
    expect(controller.state.preview).toBe(true);
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['cached-user', 'cached-answer']);

    resolve(page({ entries: [entry('fresh-user', 'user', 'fresh question')], sourceRevision: 'new-revision' }));
    await vi.waitFor(() => expect(controller.state.preview).toBe(false));
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['fresh-user', 'fresh-answer']);
  });

  it('rejects a changed snapshot identity after preparation starts', async () => {
    let calls = 0;
    const request = vi.fn(async () => {
      calls++;
      if (calls === 1) return page({
        entries: [entry('u1', 'user', 'question'), entry('a1', 'assistant', 'answer')],
        nextCursor: 'older-cursor',
        hasMore: true,
        sourceRevision: 'revision-1',
      });
      if (calls === 2) return page({
        state: 'preparing',
        mode: 'snapshot',
        snapshotId: 'snapshot-1',
        nextCursor: 'snapshot-cursor',
        hasMore: true,
        sourceRevision: 'revision-1',
      });
      return page({
        mode: 'snapshot',
        snapshotId: 'snapshot-2',
        entries: [entry('older', 'user', 'older question')],
        sourceRevision: 'revision-1',
      });
    });
    const controller = new ConversationHistoryController(agent(), {
      request,
      yieldToBrowser: async () => {},
      preparationIntervalMs: 0,
    });
    controller.start();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(1));
    controller.demandOlder();
    await vi.waitFor(() => expect(controller.state.error?.code).toBe('source_changed'));
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['u1', 'a1']);
    expect(request).toHaveBeenCalledTimes(3);
  });

  it('preserves content on a transient initial failure and exposes retry', async () => {
    const states: import('$lib/conversation-history').ConversationHistoryControllerState[] = [];
    let attempt = 0;
    const request = vi.fn(async (_target: Agent, _input: { retry?: boolean; limit?: number }) => {
      attempt++;
      if (attempt === 1) throw new Error('temporary read failure');
      return page({ entries: [entry('u1', 'user', 'question')], sourceRevision: 'r1' });
    });
    const controller = new ConversationHistoryController(agent(), {
      request,
      onState: (state) => states.push(state),
      yieldToBrowser: async () => {},
    });
    controller.start();
    await vi.waitFor(() => expect(controller.state.error?.message).toBe('temporary read failure'));
    expect(controller.state.initialOutcome).toBe('error');
    expect(controller.state.authoritative).toBe(false);
    controller.retry();
    await vi.waitFor(() => expect(controller.state.error).toBeNull());
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['u1']);
    expect(request.mock.calls[1][1]).toEqual(expect.objectContaining({ retry: true, limit: HISTORY_WIRE_PAGE_SIZE }));
    expect(states.length).toBeGreaterThan(1);
  });

  it('rejects a changed fresh source even when native IDs overlap', async () => {
    const requests: { cursor?: string }[] = [];
    const request = vi.fn(async (_target: Agent, input: { cursor?: string }) => {
      requests.push(input);
      if (requests.length === 1) return page({
        entries: [
          entry('old-source-only', 'user', 'old question'),
          entry('shared', 'user', 'shared prompt'),
          entry('old-answer', 'assistant', 'old answer'),
        ],
        sourceRevision: 'r1',
        nextCursor: 'old-cursor',
        hasMore: true,
      });
      return page({
        entries: [entry('shared', 'user', 'shared prompt'), entry('new-answer', 'assistant', 'new answer')],
        sourceRevision: 'r2',
      });
    });
    const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {} });
    controller.start();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(1));
    controller.refresh();
    await vi.waitFor(() => expect(controller.state.error?.code).toBe('source_changed'));
    expect(request).toHaveBeenCalledTimes(2);
    expect(controller.state.sourceRevision).toBe('r1');
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['old-source-only', 'shared', 'old-answer']);
    expect(controller.state.beginningReached).toBe(false);
  });

  it('bridges a disjoint refreshed head within the same source', async () => {
    const requests: { cursor?: string }[] = [];
    const oldPlan = { available: true, phases: [{ name: 'snapshot', tasks: [] }], truncated: false };
    const livePlan = { available: true, phases: [{ name: 'live', tasks: [] }], truncated: false };
    const request = vi.fn(async (_target: Agent, input: { cursor?: string }) => {
      requests.push(input);
      if (requests.length === 1) return page({
        entries: [entry('old', 'user', 'old question'), entry('old-answer', 'assistant', 'old answer')],
        sourceRevision: 'r1',
        nextCursor: 'old-cursor',
        hasMore: true,
        omoPlan: oldPlan,
      });
      if (requests.length === 2) return page({
        entries: [entry('new', 'assistant', 'new answer')],
        sourceRevision: 'r1',
        omoPlan: livePlan,
        nextCursor: 'bridge-cursor',
        hasMore: true,
      });
      return page({
        entries: [entry('shared', 'user', 'shared prompt'), entry('old', 'user', 'old question')],
        sourceRevision: 'r1',
        omoPlan: oldPlan,
      });
    });
    const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {} });
    controller.start();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(1));
    controller.refresh();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(3));
    expect(requests.map(({ cursor }) => cursor)).toEqual([undefined, undefined, 'bridge-cursor']);
    expect(controller.state.nextCursor).toBe('old-cursor');
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['shared', 'old', 'old-answer', 'new']);
    expect(controller.state.omoPlan).toEqual(livePlan);
    expect(controller.state.latestGapOutstanding).toBe(false);
  });

  it('keeps an older snapshot cursor across a compatible live refresh', async () => {
    const cursors: (string | undefined)[] = [];
    const snapshotPlan = { available: true, phases: [{ name: 'snapshot', tasks: [] }], truncated: false };
    const livePlan = { available: true, phases: [{ name: 'live', tasks: [] }], truncated: false };
    const request = vi.fn(async (_target: Agent, input: { cursor?: string }) => {
      cursors.push(input.cursor);
      if (cursors.length === 1) return page({
        entries: [entry('latest-user', 'user', 'latest question'), entry('latest-answer', 'assistant', 'latest answer')],
        nextCursor: 'tail-cursor',
        hasMore: true,
        sourceRevision: 'revision-1',
        mode: 'recent',
      });
      if (cursors.length === 2) return page({
        entries: [entry('older-user', 'user', 'older question'), entry('older-answer', 'assistant', 'older answer')],
        nextCursor: 'snapshot-cursor',
        hasMore: true,
        sourceRevision: 'revision-1',
        mode: 'snapshot',
        snapshotId: 'snapshot-1',
        omoPlan: snapshotPlan,
      });
      if (cursors.length === 3) return page({
        entries: [entry('latest-user', 'user', 'latest question'), entry('latest-answer', 'assistant', 'updated latest answer'), entry('new-user', 'user', 'new question')],
        nextCursor: 'new-tail-cursor',
        hasMore: true,
        sourceRevision: 'revision-1',
        mode: 'recent',
        omoPlan: livePlan,
      });
      return page({
        entries: [entry('earlier-user', 'user', 'earlier question'), entry('earlier-answer', 'assistant', 'earlier answer')],
        sourceRevision: 'revision-1',
        mode: 'snapshot',
        snapshotId: 'snapshot-1',
      });
    });
    const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {} });
    controller.start();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(1));
    controller.ensureMore();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
    expect(controller.state.nextCursor).toBe('snapshot-cursor');
    expect(controller.state.snapshotId).toBe('snapshot-1');

    controller.refresh();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(3));
    await vi.waitFor(() => expect(controller.state.omoPlan).toEqual(livePlan));
    expect(controller.state.nextCursor).toBe('snapshot-cursor');
    expect(controller.state.snapshotId).toBe('snapshot-1');

    controller.demandOlder();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(4));
    await vi.waitFor(() => expect(controller.state.entries.map(({ id }) => id)).toContain('earlier-user'));
    expect(cursors).toEqual([undefined, 'tail-cursor', undefined, 'snapshot-cursor']);
  });

  it('stops a repeated ready cursor as a stalled demand', async () => {
    const request = vi.fn(async () => page({
      entries: [entry('u1', 'user', 'question')],
      nextCursor: 'same-cursor',
      hasMore: true,
      sourceRevision: 'r1',
    }));
    const controller = new ConversationHistoryController(agent(), {
      request,
      maxPagesPerDemand: 10,
      yieldToBrowser: async () => {},
    });
    controller.start();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(1));
    controller.demandOlder();
    await vi.waitFor(() => expect(controller.state.error?.code).toBe('stalled'));
    expect(controller.state.error?.message).toContain('repeated cursor');
    expect(controller.state.beginningReached).toBe(false);
  });
});

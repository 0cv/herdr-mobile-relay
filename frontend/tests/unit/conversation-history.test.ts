import { describe, expect, it, vi } from 'vitest';
import { normalizeConversationPage } from '$lib/store';
import unavailablePage from '../../../contracts/fixtures/conversation_unavailable.json';
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
  it.each([
    { code: 'source_missing', wire: unavailablePage, retryable: true },
    { code: 'history_unavailable', wire: { ...unavailablePage, reason_code: undefined }, retryable: true },
    { code: 'access_denied', wire: { ...unavailablePage, error: { code: 'access_denied', message: 'Denied', retryable: false } }, retryable: false },
  ])('retains older history and honors wire error $code', async ({ code, wire, retryable }) => {
    const retained = [entry('u2', 'user'), entry('a2', 'assistant')];
    const request = vi.fn()
      .mockResolvedValueOnce(page({ entries: retained, hasMore: true, nextCursor: 'older' }))
      .mockResolvedValueOnce(normalizeConversationPage(wire))
      .mockResolvedValueOnce(page({ entries: [entry('u1', 'user'), entry('a1', 'assistant')] }));
    const controller = new ConversationHistoryController(agent(), { request });
    try {
      controller.start();
      await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
      controller.demandOlder();
      await vi.waitFor(() => expect(controller.state.errorCode).toBe(code));
      expect(controller.state.errorRetryable).toBe(retryable);
      expect(controller.state.entries).toEqual(retained);
      expect(controller.state.nextCursor).toBe('older');
      expect(controller.state.hasMore).toBe(true);
      controller.retry();
      if (!retryable) {
        expect(request).toHaveBeenCalledTimes(2);
        expect(controller.state.error?.message).toBe('Denied');
        return;
      }
      await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(3));
      expect(request.mock.calls[2][1]).toMatchObject({ cursor: 'older', retry: true });
      await vi.waitFor(() => expect(controller.state.entries.map(({ id }) => id)).toEqual(['u1', 'a1', 'u2', 'a2']));
      expect(controller.state.errorCode).toBe('');
    } finally {
      controller.cancel();
    }
  });

  it('keeps native cursor traversal working without a total and clears a stale head count', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce(page({ mode: 'native', total: 4, sourceRevision: 'db', entries: [entry('u2', 'user'), entry('a2', 'assistant')], hasMore: true, nextCursor: 'older' }))
      .mockResolvedValueOnce(page({ mode: 'native', total: null, sourceRevision: 'db', entries: [entry('u2', 'user'), entry('a2', 'assistant')], hasMore: true, nextCursor: 'older' }))
      .mockResolvedValueOnce(page({ mode: 'native', total: null, sourceRevision: 'db', entries: [entry('u1', 'user'), entry('a1', 'assistant')] }));
    const controller = new ConversationHistoryController(agent(), { request });
    controller.start();
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(controller.state.total).toBe(4);
    controller.refresh();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(controller.state.total).toBeNull();
    controller.demandOlder();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(3));
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['u1', 'a1', 'u2', 'a2']);
    expect(controller.state.hasMore).toBe(false);
    expect(controller.state.total).toBeNull();
    controller.cancel();
  });

  it.each([[1, 'live result'], [3, 'snapshot result']] as const)('orders a snapshot captured at sequence %i against a concurrent finalized delta', async (snapshotSequence, expectedOutput) => {
    const toolEntry = (output: string) => entry('pi:answer', 'assistant', 'Tool turn', [{ id: 'tool', name: 'read', output }]);
    const snapshot = (sequence: number, output: string) => page({ mode: 'snapshot', snapshotId: 'snapshot', sourceRevision: 'revision', sequence, entries: [entry('pi:user', 'user'), toolEntry(output)] });
    let resolveHead: (value: ConversationPage) => void = () => {};
    const request = vi.fn().mockResolvedValueOnce(snapshot(0, 'initial')).mockImplementationOnce(() => new Promise<ConversationPage>(resolve => { resolveHead = resolve; }));
    const controller = new ConversationHistoryController(agent(), { request });
    controller.start();
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    controller.refresh();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
    expect(controller.applyLiveEntries('revision', [toolEntry('live result')], 2)).toBe(true);
    resolveHead(snapshot(snapshotSequence, 'snapshot result'));
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(controller.state.entries.find(entry => entry.id === 'pi:answer')?.tools?.[0].output).toBe(expectedOutput);
    controller.cancel();
  });

  it('opens a bounded recent page and fetches missing prompt context only on demand', async () => {
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
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(request).toHaveBeenCalledTimes(1);
    expect(HISTORY_WIRE_PAGE_SIZE).toBe(24);
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['answer-only']);
    controller.demandOlder();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
    expect(calls.map(({ cursor, limit }) => [cursor, limit])).toEqual([
      [undefined, HISTORY_WIRE_PAGE_SIZE],
      ['context-cursor', HISTORY_WIRE_PAGE_SIZE],
    ]);
    await vi.waitFor(() => expect(controller.state.entries.map(({ id }) => id)).toEqual(['question', 'answer-only']));
    expect(controller.state.beginningReached).toBe(true);
  });

  it('bounds each older demand to one ready page including tool-only pages', async () => {
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
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(request).toHaveBeenCalledTimes(3);
    expect(controller.state.entries.map(({ id }) => id)).toContain('tool-0');
    controller.demandOlder();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(4));

    expect(requests.map(({ cursor }) => cursor)).toEqual([undefined, 'older-1', 'older-2', 'older-3']);
    await vi.waitFor(() => expect(controller.state.entries.map(({ id }) => id)).toContain('earlier-user'));
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

  it('shows tools and assistant activity without searching for a deep prompt', async () => {
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
    expect(request).toHaveBeenCalledTimes(1);
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['tool-only', 'assistant-activity']);
    expect(controller.state.contextSearching).toBe(false);
    expect(controller.state.hasMore).toBe(true);
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

  it('keeps a warm preview separate until a fresh recent fragment is accepted', async () => {
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
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(request).toHaveBeenCalledTimes(1);
    expect(controller.state.preview).toBe(false);
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['fresh-answer']);
    controller.demandOlder();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));

    resolve(page({ entries: [entry('fresh-user', 'user', 'fresh question')], sourceRevision: 'new-revision' }));
    await vi.waitFor(() => expect(controller.state.entries.map(({ id }) => id)).toEqual(['fresh-user', 'fresh-answer']));
    expect(controller.state.preview).toBe(false);
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

  it('clears a recovered continuation warning on a cursorless latest refresh', async () => {
    let calls = 0;
    const request = vi.fn(async () => {
      calls++;
      if (calls === 1) return page({
        entries: [entry('latest', 'assistant', 'latest answer')],
        sourceRevision: 'revision-1',
        diagnostics: { oversized_records: 0, corrupt_records: 0, source_truncated: false, continuation_incomplete: true, continuation_reason: 'missing_source' },
      });
      return page({
        entries: [entry('latest', 'assistant', 'latest answer'), entry('child', 'assistant', 'child answer')],
        sourceRevision: 'revision-1',
        diagnostics: { oversized_records: 0, corrupt_records: 0, source_truncated: false },
      });
    });
    const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {} });
    controller.start();
    await vi.waitFor(() => expect(controller.state.diagnostics?.continuation_incomplete).toBe(true));
    controller.refresh();
    await vi.waitFor(() => expect(controller.state.entries.map(({ id }) => id)).toContain('child'));
    expect(controller.state.diagnostics?.continuation_incomplete).not.toBe(true);
    expect(controller.state.diagnostics?.continuation_reason).toBeUndefined();
  });

  it('clears a live continuation warning without discarding the older cursor lane', async () => {
    let calls = 0;
    const request = vi.fn(async (_target: Agent, input: { cursor?: string }) => {
      calls++;
      if (calls === 1) return page({
        entries: [entry('u1', 'user', 'question'), entry('a1', 'assistant', 'latest answer')],
        sourceRevision: 'revision-1',
        nextCursor: 'older-cursor',
        hasMore: true,
        diagnostics: { oversized_records: 0, corrupt_records: 0, source_truncated: false, continuation_incomplete: true, continuation_reason: 'missing_source' },
      });
      expect(input.cursor).toBeUndefined();
      return page({
        entries: [entry('u1', 'user', 'question'), entry('a1', 'assistant', 'latest answer'), entry('c1', 'assistant', 'recovered child')],
        sourceRevision: 'revision-1',
        diagnostics: { oversized_records: 0, corrupt_records: 0, source_truncated: false },
      });
    });
    const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {} });
    controller.start();
    await vi.waitFor(() => expect(controller.state.diagnostics?.continuation_incomplete).toBe(true));
    controller.refresh();
    await vi.waitFor(() => expect(controller.state.entries.map(({ id }) => id)).toContain('c1'));
    expect(controller.state.diagnostics?.continuation_incomplete).not.toBe(true);
    expect(controller.state.diagnostics?.continuation_reason).toBeUndefined();
    expect(controller.state.nextCursor).toBe('older-cursor');
    expect(controller.state.hasMore).toBe(true);
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

  it('bridges new replies after scrolling all the way to the beginning', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce(page({
        entries: [entry('question', 'user', 'question')],
        sourceRevision: 'r1', nextCursor: 'older', hasMore: true,
      }))
      .mockResolvedValueOnce(page({
        entries: [entry('first', 'user', 'first question')],
        sourceRevision: 'r1', mode: 'snapshot', snapshotId: 'snapshot-1',
      }))
      .mockResolvedValueOnce(page({
        entries: [entry('new', 'assistant', 'new answer')],
        sourceRevision: 'r1', mode: 'recent', nextCursor: 'bridge', hasMore: true,
      }))
      .mockResolvedValueOnce(page({
        entries: [entry('question', 'user', 'question'), entry('between', 'assistant', 'intermediate answer')],
        sourceRevision: 'r1', mode: 'recent',
      }));
    const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {} });
    controller.start();
    await vi.waitFor(() => expect(controller.state.authoritative).toBe(true));
    controller.demandOlder();
    await vi.waitFor(() => expect(controller.state.beginningReached).toBe(true));
    controller.refresh();
    await vi.waitFor(() => expect(controller.state.entries.map(({ id }) => id)).toEqual(['first', 'question', 'between', 'new']));
    expect(request.mock.calls.map(([, input]) => input.cursor)).toEqual([undefined, 'older', undefined, 'bridge']);
    expect(controller.state.beginningReached).toBe(true);
    expect(controller.state.nextCursor).toBe('');
    expect(controller.state.latestGapOutstanding).toBe(false);
    controller.cancel();
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
        entries: Array.from({ length: 12 }, (_, index) => entry(`older-${index}`, 'user', `older question ${index}`)),
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
    controller.demandOlder();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
    expect(controller.state.intent).toBe('historical');
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

describe('conversation clone values through history', () => {
  type Plan = import('$lib/types').OmoTodoState;
  type Preview = import('$lib/conversation-cache').ConversationPreview;
  type State = import('$lib/conversation-history').ConversationHistoryControllerState;
  type Graph = { entries: ConversationEntry[]; plan: Plan | null | undefined };

  function fixture() {
    const sentinels = {
      entry: { label: 'entry extension' }, tool: { label: 'tool extension' },
      plan: { label: 'plan extension' }, phase: { label: 'phase extension' }, task: { label: 'task extension' },
    };
    const tool = { id: 'tool', name: 'Read', input: 'file', output: 'tool result', extension: sentinels.tool };
    const entries = [entry('u', 'user', 'question'), { ...entry('a', 'assistant', 'answer', [tool]), extension: sentinels.entry }];
    const task = { content: 'task', status: 'pending' as Plan['phases'][number]['tasks'][number]['status'], extension: sentinels.task };
    const phase = { name: 'phase', tasks: [task], extension: sentinels.phase };
    const plan = { available: true, phases: [phase], truncated: false, extension: sentinels.plan };
    return { entries, plan, sentinels };
  }

  function expectValues(graph: Graph, sentinels: ReturnType<typeof fixture>['sentinels']) {
    expect(graph.entries.map(({ id, role, text }) => [id, role, text])).toEqual([['u', 'user', 'question'], ['a', 'assistant', 'answer']]);
    expect(graph.entries[1].tools).toHaveLength(1);
    expect(graph.entries[1].tools![0]).toMatchObject({ id: 'tool', name: 'Read', input: 'file', output: 'tool result' });
    expect(graph.plan).toMatchObject({ available: true, truncated: false, phases: [{ name: 'phase', tasks: [{ content: 'task', status: 'pending' }] }] });
    const extension = (value: object) => (value as { extension?: object }).extension;
    expect(extension(graph.entries[1])).toBe(sentinels.entry);
    expect(extension(graph.entries[1].tools![0])).toBe(sentinels.tool);
    expect(extension(graph.plan!)).toBe(sentinels.plan);
    expect(extension(graph.plan!.phases[0])).toBe(sentinels.phase);
    expect(extension(graph.plan!.phases[0].tasks[0])).toBe(sentinels.task);
  }

  // Only the explicitly cloned entry/tool/plan targets, not arbitrary nested properties.
  function targets(graph: Graph): object[] {
    return [graph.entries, ...graph.entries, graph.entries[1].tools!, graph.entries[1].tools![0],
      graph.plan!, graph.plan!.phases, graph.plan!.phases[0], graph.plan!.phases[0].tasks, graph.plan!.phases[0].tasks[0]];
  }

  function expectIndependent(graphs: Graph[]) {
    const mapped = graphs.map(targets);
    for (let index = 0; index < mapped[0].length; index++) {
      expect(new Set(mapped.map((values) => values[index])).size).toBe(graphs.length);
    }
  }

  function mutate(graph: Graph) {
    graph.entries[0].text = 'changed question';
    graph.entries[1].text = 'changed answer';
    graph.entries[1].tools![0].output = 'changed output';
    graph.plan!.available = false;
    graph.plan!.phases[0].name = 'changed phase';
    graph.plan!.phases[0].tasks[0].content = 'changed task';
    graph.plan!.phases[0].tasks[0].status = 'completed';
  }

  it('isolates recorded entry and plan clone channels', async () => {
    const source = fixture();
    const recorded = page({ entries: source.entries, omoPlan: source.plan, sourceRevision: 'r' });
    const before = JSON.stringify(recorded);
    const states: State[] = [];
    const saved: Preview[] = [];
    const controller = new ConversationHistoryController(agent(), {
      request: async () => recorded,
      onState: (state) => states.push(state),
      putPreview: (value) => saved.push(value),
    });
    try {
      controller.start();
      await vi.waitFor(() => {
        expect(controller.state.authoritative).toBe(true);
        expect(controller.state.requestPhase).toBe('idle');
      });
      const first = controller.state;
      const second = controller.state;
      const callback = states.at(-1)!;
      expect(saved).toHaveLength(1);
      expect(callback.authoritative).toBe(true);
      expect(callback.requestPhase).toBe('idle');
      expect(first.state).toBe('ready');
      expect(first.sourceRevision).toBe('r');
      expect(first.error).toBeNull();
      const graphs: Graph[] = [
        { entries: source.entries, plan: source.plan },
        { entries: first.entries, plan: first.omoPlan },
        { entries: first.lastPage!.entries, plan: first.lastPage!.omoPlan },
        { entries: second.entries, plan: second.omoPlan },
        { entries: second.lastPage!.entries, plan: second.lastPage!.omoPlan },
        { entries: callback.entries, plan: callback.omoPlan },
        { entries: callback.lastPage!.entries, plan: callback.lastPage!.omoPlan },
        { entries: saved[0].entries, plan: saved[0].plan },
      ];
      for (const graph of graphs) expectValues(graph, source.sentinels);
      expectIndependent(graphs);
      expect(first.lastPage).not.toBe(recorded);
      expect(first.lastPage).not.toBe(second.lastPage);
      expect(callback.lastPage).not.toBe(first.lastPage);
      expect(JSON.stringify(recorded)).toBe(before);
      for (const graph of graphs) mutate(graph);
      const fresh = controller.state;
      expectValues({ entries: fresh.entries, plan: fresh.omoPlan }, source.sentinels);
      expectValues({ entries: fresh.lastPage!.entries, plan: fresh.lastPage!.omoPlan }, source.sentinels);
    } finally {
      controller.cancel();
    }
  });

  it('isolates hydrated preview entries and plan', async () => {
    const source = fixture();
    const warm: Preview = {
      identity: conversationIdentity(agent())!, relayId: 'relay-1', savedAt: 100,
      sourceRevision: 'warm', historical: false, entries: source.entries, plan: source.plan,
    };
    const before = JSON.stringify(warm);
    let settle!: (value: ConversationPage) => void;
    const pending = new Promise<ConversationPage>((resolve) => { settle = resolve; });
    const request = vi.fn(() => pending);
    const controller = new ConversationHistoryController(agent(), { request, getPreview: () => warm });
    try {
      controller.start();
      expect(request).toHaveBeenCalledTimes(1);
      const first = controller.state;
      const second = controller.state;
      expect(first.preview).toBe(true);
      expect(first.authoritative).toBe(false);
      expect(first.requestPhase).toBe('initial');
      expect(first.sourceRevision).toBe('warm');
      const graphs: Graph[] = [source, { entries: first.entries, plan: first.omoPlan }, { entries: second.entries, plan: second.omoPlan }];
      for (const graph of graphs) expectValues(graph, source.sentinels);
      expectIndependent(graphs);
      expect(JSON.stringify(warm)).toBe(before);
      mutate(graphs[0]);
      mutate(graphs[1]);
      expectValues(graphs[2], source.sentinels);
      const fresh = controller.state;
      expect(fresh.preview).toBe(true);
      expect(fresh.authoritative).toBe(false);
      expectValues({ entries: fresh.entries, plan: fresh.omoPlan }, source.sentinels);
    } finally {
      controller.cancel();
      settle(page());
      await pending;
      await Promise.resolve();
    }
  });

  it('preserves live tool property presence', async () => {
    const empty: NonNullable<ConversationEntry['tools']> = [];
    const live: ConversationEntry[] = [
      entry('absent', 'user'),
      { ...entry('undefined', 'user'), tools: undefined },
      { ...entry('null', 'user'), tools: null as unknown as ConversationEntry['tools'] },
      { ...entry('empty', 'user'), tools: empty },
    ];
    const controller = new ConversationHistoryController(agent(), { request: async () => page({ sourceRevision: 'r' }) });
    try {
      controller.start();
      await vi.waitFor(() => {
        expect(controller.state.authoritative).toBe(true);
        expect(controller.state.requestPhase).toBe('idle');
      });
      expect(controller.applyLiveEntries('r', live, 1)).toBe(true);
      const first = controller.state;
      expect(first.entries.map(({ id }) => id)).toEqual(['absent', 'undefined', 'null', 'empty']);
      expect(first.omoPlan).toBeNull();
      for (const values of [live, first.entries]) {
        expect(values.map((value) => Object.hasOwn(value, 'tools'))).toEqual([false, true, true, true]);
        expect(values[0].tools).toBeUndefined();
        expect(values[1].tools).toBeUndefined();
        expect(values[2].tools).toBeNull();
        expect(values[3].tools).toEqual([]);
        expect(values.map(({ text }) => text)).toEqual(['absent', 'undefined', 'null', 'empty']);
      }
      for (let index = 0; index < live.length; index++) expect(first.entries[index]).not.toBe(live[index]);
      expect(first.entries).not.toBe(live);
      expect(first.entries[3].tools).not.toBe(empty);
      live[0].text = 'changed input';
      empty.push({ name: 'changed input tool' });
      first.entries[1].text = 'changed outward';
      first.entries[3].tools!.push({ name: 'changed outward tool' });
      const fresh = controller.state;
      expect(fresh.entries.map(({ text }) => text)).toEqual(['absent', 'undefined', 'null', 'empty']);
      expect(fresh.entries.map((value) => Object.hasOwn(value, 'tools'))).toEqual([false, true, true, true]);
      expect(fresh.entries[1].tools).toBeUndefined();
      expect(fresh.entries[2].tools).toBeNull();
      expect(fresh.entries[3].tools).toEqual([]);
      expect(fresh.entries[3].tools).not.toBe(first.entries[3].tools);
      expect(fresh.omoPlan).toBeNull();
    } finally {
      controller.cancel();
    }
  });

  it('preserves batch entry getter evaluation order', () => {
    const trace: string[] = [];
    const tool = { name: 'Read', get output() { trace.push('tool.output'); return 'tool result'; } };
    const tools = [tool];
    const source = {
      ...entry('getter', 'user', 'question'),
      get tools() { trace.push('entry.tools'); return tools; },
      get after() { trace.push('entry.after'); return 'after'; },
    };
    const entries = [source];
    const result = analyzeConversationBatch(entries);
    expect(trace).toEqual(['entry.tools', 'entry.after', 'entry.tools', 'entry.tools', 'tool.output']);
    expect(result.displayableEntries).not.toBe(entries);
    const copied = result.displayableEntries[0];
    expect(copied).not.toBe(source);
    expect(copied.id).toBe('getter');
    expect(copied.text).toBe('question');
    expect((copied as ConversationEntry & { after: string }).after).toBe('after');
    expect(copied.tools).not.toBe(tools);
    expect(copied.tools).toHaveLength(1);
    expect(copied.tools![0]).not.toBe(tool);
    expect(copied.tools![0].name).toBe('Read');
    expect(copied.tools![0].output).toBe('tool result');
  });
});

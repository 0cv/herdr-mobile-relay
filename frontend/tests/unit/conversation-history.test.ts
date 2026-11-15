import { afterEach, describe, expect, it, vi } from 'vitest';
import { normalizeConversationPage } from '$lib/conversation-page';
import unavailablePage from '../../../contracts/fixtures/conversation_unavailable.json';
import {
  conversationIdentity,
  ConversationHistoryController,
  HISTORY_ACTIVE_DEADLINE_MS,
  HISTORY_WIRE_PAGE_SIZE,
} from '$lib/conversation-history';
import type { Agent, ConversationEntry, ConversationHistoryRequest, ConversationPage } from '$lib/types';

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

describe('conversation history identity', () => {
  it('uses exact target identity while ignoring presentation metadata', () => {
    const first = agent();
    const changed = { ...first, status: 'waiting', project: 'Renamed project', relay_label: 'New label' };
    expect(conversationIdentity(first)).toBe(conversationIdentity(changed));
    expect(conversationIdentity({ ...first, raw_pane_id: 'pane-2' })).not.toBe(conversationIdentity(first));
    expect(conversationIdentity({ ...first, agent_session_id: '' })).toBeNull();
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
      await vi.waitFor(() => expect(controller.state.error?.code).toBe(code));
      expect(controller.state.error?.retryable).toBe(retryable);
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
      expect(controller.state.error).toBeNull();
    } finally {
      controller.cancel();
    }
  });

  it('keeps native cursor traversal working without a total', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce(page({ mode: 'native', total: 4, sourceRevision: 'db', entries: [entry('u2', 'user'), entry('a2', 'assistant')], hasMore: true, nextCursor: 'older' }))
      .mockResolvedValueOnce(page({ mode: 'native', total: null, sourceRevision: 'db', entries: [entry('u2', 'user'), entry('a2', 'assistant')], hasMore: true, nextCursor: 'older' }))
      .mockResolvedValueOnce(page({ mode: 'native', total: null, sourceRevision: 'db', entries: [entry('u1', 'user'), entry('a1', 'assistant')] }));
    const controller = new ConversationHistoryController(agent(), { request });
    controller.start();
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    controller.refresh();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    controller.demandOlder();
    await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(3));
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['u1', 'a1', 'u2', 'a2']);
    expect(controller.state.hasMore).toBe(false);
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
    expect(controller.state.authoritative).toBe(false);
    controller.retry();
    await vi.waitFor(() => expect(controller.state.error).toBeNull());
    expect(controller.state.entries.map(({ id }) => id)).toEqual(['u1']);
    expect(request.mock.calls[1][1]).toEqual(expect.objectContaining({ retry: true, limit: HISTORY_WIRE_PAGE_SIZE }));
    expect(states.length).toBeGreaterThan(1);
  });

  it.each([
    { code: 'memory_limit', retryable: true },
    { code: 'stalled', retryable: true },
    { code: 'work_limit', retryable: true },
    { code: 'source_changed', retryable: true },
    { code: 'access_denied', retryable: false },
    { code: 'transport_disconnected', retryable: false },
  ])('does not recover $code with retryable=$retryable on reconnect', async ({ code, retryable }) => {
    const request = vi.fn().mockRejectedValue({ error: { code, message: 'History needs attention', retryable } });
    const controller = new ConversationHistoryController(agent(), { request });
    controller.start();
    await vi.waitFor(() => expect(controller.state.error?.code).toBe(code));
    expect(controller.recoverAfterReconnect(agent())).toBe(false);
    expect(request).toHaveBeenCalledOnce();
    controller.cancel();
  });

  it('recovers transport failures only for the active current target and without parallel requests', async () => {
    let active = true;
    let finish!: (value: ConversationPage) => void;
    const pending = new Promise<ConversationPage>((resolve) => { finish = resolve; });
    const request = vi.fn().mockRejectedValueOnce({ error: { code: 'transport_disconnected', message: 'Relay is not connected', retryable: true } }).mockReturnValue(pending);
    const controller = new ConversationHistoryController(agent(), { request, isActive: () => active });
    controller.start();
    await vi.waitFor(() => expect(controller.state.error?.code).toBe('transport_disconnected'));
    expect(controller.recoverAfterReconnect({ ...agent(), generation: 2 })).toBe(false);
    active = false;
    expect(controller.recoverAfterReconnect(agent())).toBe(false);
    active = true;
    controller.pause();
    expect(controller.state.pausedReason).toBeTruthy();
    expect(controller.recoverAfterReconnect(agent())).toBe(false);
    controller.resume();
    expect(controller.recoverAfterReconnect(agent())).toBe(true);
    expect(controller.recoverAfterReconnect(agent())).toBe(false);
    expect(request).toHaveBeenCalledTimes(2);
    expect(request.mock.calls[1][1]).toMatchObject({ retry: true, limit: HISTORY_WIRE_PAGE_SIZE });
    expect(request.mock.calls[1][1].cursor).toBeUndefined();
    finish(page({ entries: [entry('latest', 'assistant')], sourceRevision: 'r1' }));
    await vi.waitFor(() => expect(controller.state.error).toBeNull());
    controller.cancel();
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

describe('conversation history entry ownership', () => {
  type Plan = import('$lib/types').OmoTodoState;
  type Preview = import('$lib/conversation-cache').ConversationPreview;
  type State = import('$lib/conversation-history').ConversationHistoryControllerState;

  function recordedPlan(): Plan {
    return { available: true, phases: [{ name: 'phase', tasks: [{ content: 'task', status: 'pending' }] }], truncated: false };
  }

  function recordedEntries(): ConversationEntry[] {
    return [entry('u', 'user', 'question'), entry('a', 'assistant', 'answer', [{ id: 'tool', name: 'Read', input: 'file', output: 'tool result' }])];
  }

  function expectRecordedValues(entries: ConversationEntry[], plan: Plan | null | undefined) {
    expect(entries.map(({ id, role, text }) => [id, role, text])).toEqual([['u', 'user', 'question'], ['a', 'assistant', 'answer']]);
    expect(entries[1].tools).toEqual([{ id: 'tool', name: 'Read', input: 'file', output: 'tool result' }]);
    expect(plan).toEqual(recordedPlan());
  }

  it('publishes an accepted page that later changes to the received page do not alter', async () => {
    const received = page({ entries: recordedEntries(), omoPlan: recordedPlan(), sourceRevision: 'r' });
    const states: State[] = [];
    const saved: Preview[] = [];
    const controller = new ConversationHistoryController(agent(), {
      request: async () => received,
      onState: (state) => states.push(state),
      putPreview: (value) => saved.push(value),
    });
    try {
      controller.start();
      await vi.waitFor(() => {
        expect(controller.state.authoritative).toBe(true);
        expect(controller.state.requestPhase).toBe('idle');
      });
      received.entries[0].text = 'changed question';
      received.entries[1].tools![0].output = 'changed output';
      received.omoPlan!.phases[0].tasks[0].content = 'changed task';
      const published = controller.state;
      expectRecordedValues(published.entries, published.omoPlan);
      expectRecordedValues(states.at(-1)!.entries, states.at(-1)!.omoPlan);
      expect(saved).toHaveLength(1);
      expectRecordedValues(saved[0].entries, saved[0].plan);
    } finally {
      controller.cancel();
    }
  });

  it('keeps earlier published states unchanged when later batches arrive', async () => {
    const states: State[] = [];
    const controller = new ConversationHistoryController(agent(), {
      request: async () => page({ entries: [entry('u', 'user', 'question')], sourceRevision: 'r' }),
      onState: (state) => states.push(state),
    });
    try {
      controller.start();
      await vi.waitFor(() => {
        expect(controller.state.authoritative).toBe(true);
        expect(controller.state.requestPhase).toBe('idle');
      });
      const settled = states.at(-1)!;
      expect(controller.applyLiveEntries('r', [entry('u', 'user', 'question'), entry('a', 'assistant', 'answer')], 1)).toBe(true);
      expect(settled.entries.map(({ id }) => id)).toEqual(['u']);
      expect(controller.state.entries.map(({ id }) => id)).toEqual(['u', 'a']);
      expect(states.at(-1)!.entries.map(({ id }) => id)).toEqual(['u', 'a']);
    } finally {
      controller.cancel();
    }
  });

  it('shows a warm preview until the fresh page arrives', async () => {
    const warm: Preview = {
      identity: conversationIdentity(agent())!, relayId: 'relay-1', savedAt: 100,
      sourceRevision: 'warm', historical: false, entries: recordedEntries(), plan: recordedPlan(),
    };
    let settle!: (value: ConversationPage) => void;
    const pending = new Promise<ConversationPage>((resolve) => { settle = resolve; });
    const request = vi.fn(() => pending);
    const controller = new ConversationHistoryController(agent(), { request, getPreview: () => warm });
    try {
      controller.start();
      expect(request).toHaveBeenCalledTimes(1);
      const shown = controller.state;
      expect(shown.preview).toBe(true);
      expect(shown.authoritative).toBe(false);
      expect(shown.requestPhase).toBe('initial');
      expect(shown.sourceRevision).toBe('warm');
      expectRecordedValues(shown.entries, shown.omoPlan);
    } finally {
      controller.cancel();
      settle(page());
      await pending;
      await Promise.resolve();
    }
  });

  it('keeps live entries independent of the batch that delivered them and preserves tool presence', async () => {
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
      live[0].text = 'changed input';
      empty.push({ name: 'changed input tool' });
      const published = controller.state;
      expect(published.omoPlan).toBeNull();
      expect(published.entries.map(({ id }) => id)).toEqual(['absent', 'undefined', 'null', 'empty']);
      expect(published.entries.map(({ text }) => text)).toEqual(['absent', 'undefined', 'null', 'empty']);
      expect(published.entries.map((value) => Object.hasOwn(value, 'tools'))).toEqual([false, true, true, true]);
      expect(published.entries[1].tools).toBeUndefined();
      expect(published.entries[2].tools).toBeNull();
      expect(published.entries[3].tools).toEqual([]);
    } finally {
      controller.cancel();
    }
  });

  it('stops at the memory limit without changing the loaded messages', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce(page({ entries: [entry('u2', 'user'), entry('a2', 'assistant')], hasMore: true, nextCursor: 'older', sourceRevision: 'r' }))
      .mockResolvedValueOnce(page({ entries: [entry('u1', 'user'), entry('a1', 'assistant')], sourceRevision: 'r' }));
    const controller = new ConversationHistoryController(agent(), { request, maxContentBytes: 400 });
    try {
      controller.start();
      await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
      controller.demandOlder();
      await vi.waitFor(() => expect(controller.state.error?.code).toBe('memory_limit'));
      expect(controller.state.error?.retryable).toBe(false);
      expect(controller.state.entries.map(({ id }) => id)).toEqual(['u2', 'a2']);
      expect(controller.state.nextCursor).toBe('older');
      expect(controller.state.hasMore).toBe(true);
      expect(controller.state.beginningReached).toBe(false);
    } finally {
      controller.cancel();
    }
  });
});

describe('history rejection paths', () => {
  type State = import('$lib/conversation-history').ConversationHistoryControllerState;
  type Preview = import('$lib/conversation-cache').ConversationPreview;
  type ConversationRequest = import('$lib/conversation-history').ConversationHistoryControllerOptions['request'];

  const established = () => [entry('u2', 'user'), entry('a2', 'assistant')];
  const establishedIds = ['u2', 'a2'];
  const ids = (state: State) => state.entries.map(({ id }) => id);

  function bulkEntries(prefix: string, count: number): ConversationEntry[] {
    return Array.from({ length: count }, (_, index) => entry(`${prefix}${index}`, 'assistant', 'x'));
  }

  function warmPreview(entries: ConversationEntry[]): () => Preview {
    return () => ({
      identity: conversationIdentity(agent())!,
      relayId: 'relay-1',
      savedAt: 1,
      sourceRevision: 'warm',
      historical: false,
      entries,
    });
  }

  async function loadWindow(request: ConversationRequest) {
    const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {} });
    controller.start();
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(ids(controller.state)).toEqual(establishedIds);
    return controller;
  }

  async function demandOlderRejected(request: ConversationRequest) {
    const controller = await loadWindow(request);
    controller.demandOlder();
    await vi.waitFor(() => expect(controller.state.error).not.toBeNull());
    return controller;
  }

  function expectWindowKept(state: State, code: string, retryable: boolean) {
    expect(state.error?.code).toBe(code);
    expect(state.error?.retryable).toBe(retryable);
    expect(ids(state)).toEqual(establishedIds);
    expect(state.nextCursor).toBe('older');
    expect(state.hasMore).toBe(true);
    expect(state.beginningReached).toBe(false);
  }

  const firstPage = () => page({ entries: established(), hasMore: true, nextCursor: 'older', sourceRevision: 'r1' });

  it('rejects an older page whose source revision changed', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce(firstPage())
      .mockResolvedValueOnce(page({ entries: [entry('u1', 'user')], sourceRevision: 'r2' }));
    const controller = await demandOlderRejected(request);
    try {
      expectWindowKept(controller.state, 'source_changed', false);
      expect(controller.state.error?.message).toBe('The conversation source changed while history was being browsed.');
      expect(controller.state.sourceRevision).toBe('r1');
    } finally {
      controller.cancel();
    }
  });

  it('restores the established window when a bridge continuation changes source', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce(firstPage())
      .mockResolvedValueOnce(page({ entries: [entry('new', 'assistant')], hasMore: true, nextCursor: 'bridge', sourceRevision: 'r1' }))
      .mockResolvedValueOnce(page({ entries: [entry('u2', 'user')], sourceRevision: 'r2' }));
    const controller = await loadWindow(request);
    try {
      controller.refresh();
      await vi.waitFor(() => expect(controller.state.error).not.toBeNull());
      expect(request).toHaveBeenCalledTimes(3);
      expect(request.mock.calls[2][1]).toMatchObject({ cursor: 'bridge' });
      expectWindowKept(controller.state, 'source_changed', false);
      expect(controller.state.error?.message).toBe('The conversation source changed while history was being browsed.');
      expect(controller.state.latestGapOutstanding).toBe(false);
    } finally {
      controller.cancel();
    }
  });

  it('rejects a snapshot page without a snapshot id', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce(firstPage())
      .mockResolvedValueOnce(page({ entries: [entry('u1', 'user')], mode: 'snapshot', sourceRevision: 'r1' }));
    const controller = await demandOlderRejected(request);
    try {
      expectWindowKept(controller.state, 'invalid_page', false);
      expect(controller.state.error?.message).toBe('Conversation history returned an incomplete snapshot identity.');
    } finally {
      controller.cancel();
    }
  });

  it('rejects a page that has more history but no cursor and does not retry it', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce(firstPage())
      .mockResolvedValueOnce(page({ entries: [entry('u1', 'user')], hasMore: true, sourceRevision: 'r1' }));
    const controller = await demandOlderRejected(request);
    try {
      expectWindowKept(controller.state, 'invalid_page', false);
      expect(controller.state.error?.message).toBe('Conversation history returned an incomplete continuation.');
      controller.retry();
      expect(request).toHaveBeenCalledTimes(2);
    } finally {
      controller.cancel();
    }
  });

  it('reports a failed page without an error object using the page reason and does not retry it', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce(firstPage())
      .mockResolvedValueOnce(page({ state: 'failed', reason: 'The transcript could not be read.', sourceRevision: 'r1' }));
    const controller = await demandOlderRejected(request);
    try {
      expectWindowKept(controller.state, 'history_failed', false);
      expect(controller.state.error?.message).toBe('The transcript could not be read.');
      controller.retry();
      expect(request).toHaveBeenCalledTimes(2);
    } finally {
      controller.cancel();
    }
  });

  it('stops an older page that takes the retained entries over the entry limit', async () => {
    const request = vi.fn()
      .mockResolvedValueOnce(firstPage())
      .mockResolvedValueOnce(page({ entries: bulkEntries('older-', 20_000), sourceRevision: 'r1' }));
    const controller = await demandOlderRejected(request);
    try {
      expectWindowKept(controller.state, 'memory_limit', false);
    } finally {
      controller.cancel();
    }
  });

  it('refuses live entries that take the retained entries over the entry limit', async () => {
    const request = vi.fn().mockResolvedValueOnce(page({ entries: established(), hasMore: true, nextCursor: 'older', sourceRevision: 'r1' }));
    const controller = await loadWindow(request);
    try {
      expect(controller.applyLiveEntries('r1', bulkEntries('live-', 20_000), 1)).toBe(false);
      expectWindowKept(controller.state, 'memory_limit', false);
    } finally {
      controller.cancel();
    }
  });

  it('keeps a warm preview when the fresh page alone is over the entry limit', async () => {
    const cached = [entry('cached-user', 'user'), entry('cached-answer', 'assistant')];
    const request = vi.fn().mockResolvedValue(page({ entries: bulkEntries('fresh-', 20_001), sourceRevision: 'r1' }));
    const controller = new ConversationHistoryController(agent(), { request, getPreview: warmPreview(cached) });
    try {
      controller.start();
      await vi.waitFor(() => expect(controller.state.error?.code).toBe('memory_limit'));
      expect(controller.state.error?.retryable).toBe(false);
      expect(ids(controller.state)).toEqual(['cached-user', 'cached-answer']);
      expect(controller.state.preview).toBe(true);
      expect(controller.state.authoritative).toBe(false);
      expect(controller.state.nextCursor).toBe('');
      expect(controller.state.hasMore).toBe(false);
    } finally {
      controller.cancel();
    }
  });

  it('counts only the fresh page against the entry limit while a warm preview is visible', async () => {
    const cached = [entry('cached-user', 'user'), entry('cached-answer', 'assistant')];
    const request = vi.fn().mockResolvedValue(page({ entries: bulkEntries('fresh-', 20_000), sourceRevision: 'r1' }));
    const controller = new ConversationHistoryController(agent(), { request, getPreview: warmPreview(cached) });
    try {
      controller.start();
      await vi.waitFor(() => expect(controller.state.authoritative).toBe(true));
      expect(controller.state.error).toBeNull();
      expect(controller.state.preview).toBe(false);
      expect(controller.state.entries).toHaveLength(20_000);
    } finally {
      controller.cancel();
    }
  });

  it('keeps a warm preview and offers retry when the fresh page is empty and truncated', async () => {
    const cached = [entry('cached-user', 'user'), entry('cached-answer', 'assistant')];
    const truncated = page({
      sourceRevision: 'r1',
      diagnostics: { oversized_records: 0, corrupt_records: 0, source_truncated: true },
    });
    const request = vi.fn()
      .mockResolvedValueOnce(truncated)
      .mockResolvedValueOnce(page({ entries: [entry('fresh-user', 'user')], sourceRevision: 'r1' }));
    const controller = new ConversationHistoryController(agent(), { request, getPreview: warmPreview(cached) });
    try {
      controller.start();
      await vi.waitFor(() => expect(controller.state.error?.code).toBe('incomplete_history'));
      expect(controller.state.error?.retryable).toBe(true);
      expect(controller.state.preview).toBe(true);
      expect(controller.state.authoritative).toBe(false);
      expect(ids(controller.state)).toEqual(['cached-user', 'cached-answer']);
      controller.retry();
      await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
      expect(request.mock.calls[1][1].cursor).toBeUndefined();
      expect(request.mock.calls[1][1]).toMatchObject({ retry: true });
      await vi.waitFor(() => expect(controller.state.authoritative).toBe(true));
      expect(ids(controller.state)).toEqual(['fresh-user']);
    } finally {
      controller.cancel();
    }
  });

  it('replaces a warm preview with a cursorless unavailable page', async () => {
    const cached = [entry('cached-user', 'user'), entry('cached-answer', 'assistant')];
    const unavailable = page({ available: false, reasonCode: 'source_missing', reason: 'The transcript is gone.' });
    const onInitialPage = vi.fn();
    const controller = new ConversationHistoryController(agent(), {
      request: vi.fn().mockResolvedValue(unavailable),
      getPreview: warmPreview(cached),
      onInitialPage,
    });
    try {
      controller.start();
      expect(ids(controller.state)).toEqual(['cached-user', 'cached-answer']);
      await vi.waitFor(() => expect(controller.state.available).toBe(false));
      expect(controller.state.entries).toEqual([]);
      expect(controller.state.authoritative).toBe(true);
      expect(controller.state.preview).toBe(false);
      expect(controller.state.nextCursor).toBe('');
      expect(controller.state.hasMore).toBe(false);
      expect(controller.state.error).toBeNull();
      expect(onInitialPage).toHaveBeenCalledTimes(1);
    } finally {
      controller.cancel();
    }
  });
});

describe('history runaway guards', () => {
  type ControllerOptions = import('$lib/conversation-history').ConversationHistoryControllerOptions;

  afterEach(() => {
    vi.useRealTimers();
  });

  const windowIds = ['u2', 'a2'];
  const ids = (controller: ConversationHistoryController) => controller.state.entries.map(({ id }) => id);
  const windowPage = () => page({
    entries: [entry('u2', 'user'), entry('a2', 'assistant')],
    hasMore: true,
    nextCursor: 'older',
    sourceRevision: 'r1',
  });
  const bridgeStep = (step: number, entries = [entry(`b${step}`, 'assistant', 'x')]) => page({
    entries,
    hasMore: true,
    nextCursor: `c${step}`,
    sourceRevision: 'r1',
  });

  function requestAfterWindow(reply: (call: number) => ConversationPage | Promise<ConversationPage>) {
    let calls = 0;
    return vi.fn(async (_target: Agent, _input: ConversationHistoryRequest) => (++calls === 1 ? windowPage() : reply(calls)));
  }

  function newController(request: ReturnType<typeof requestAfterWindow>, options: Partial<ControllerOptions> = {}) {
    return new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {}, ...options });
  }

  async function loadWindow(request: ReturnType<typeof requestAfterWindow>, options: Partial<ControllerOptions> = {}) {
    const controller = newController(request, options);
    controller.start();
    await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
    expect(ids(controller)).toEqual(windowIds);
    return controller;
  }

  async function bridgeStops(request: ReturnType<typeof requestAfterWindow>, options: Partial<ControllerOptions> = {}) {
    const controller = await loadWindow(request, options);
    controller.refresh();
    await vi.waitFor(() => expect(controller.state.error).not.toBeNull(), { timeout: 5000 });
    return controller;
  }

  function expectLatestPageRequested(request: ReturnType<typeof requestAfterWindow>, callIndex: number) {
    expect(request.mock.calls[callIndex][1].cursor).toBeUndefined();
    expect(request.mock.calls[callIndex][1]).toMatchObject({ retry: true });
  }

  it('pauses a bridge that keeps finding new records after the page limit and retries from the latest page', async () => {
    const request = requestAfterWindow(call => bridgeStep(call));
    const controller = await bridgeStops(request, { maxPagesPerDemand: 3 });
    try {
      expect(controller.state.error?.code).toBe('work_limit');
      expect(controller.state.error?.retryable).toBe(true);
      expect(request).toHaveBeenCalledTimes(4);
      expect(ids(controller)).toEqual(windowIds);
      controller.retry();
      await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(5));
      expectLatestPageRequested(request, 4);
      expect(ids(controller)).toEqual(windowIds);
    } finally {
      controller.cancel();
    }
  });

  describe('preparation', () => {
    const preparing = (progress: number) => page({
      state: 'preparing',
      hasMore: true,
      nextCursor: 'prepared',
      sourceRevision: 'r1',
      progress: { phase: 'scanning', scanned_bytes: progress, source_bytes: 1_000_000 },
    });

    it('stops after 30 preparing pages with the same progress and continues from the same cursor', async () => {
      let calls = 0;
      const request = vi.fn(async (_target: Agent, _input: ConversationHistoryRequest) => {
        calls++;
        return calls <= 30 ? preparing(10) : page({ entries: [entry('u1', 'user'), entry('a1', 'assistant')], sourceRevision: 'r1' });
      });
      const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {}, preparationIntervalMs: 0 });
      try {
        controller.start();
        await vi.waitFor(() => expect(controller.state.error?.code).toBe('preparation_stalled'));
        expect(controller.state.error?.retryable).toBe(true);
        expect(controller.state.preparationPolls).toBe(30);
        expect(request).toHaveBeenCalledTimes(30);
        controller.continuePreparation();
        expect(controller.state.preparationPolls).toBe(0);
        await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(31));
        expect(request.mock.calls[30][1].cursor).toBe('prepared');
        await vi.waitFor(() => expect(ids(controller)).toEqual(['u1', 'a1']));
        expect(controller.state.error).toBeNull();
      } finally {
        controller.cancel();
      }
    });

    it('keeps preparing past 30 pages while the progress changes', async () => {
      let calls = 0;
      const request = vi.fn(async (_target: Agent, _input: ConversationHistoryRequest) => {
        calls++;
        return calls <= 35 ? preparing(calls) : page({ entries: [entry('u1', 'user'), entry('a1', 'assistant')], sourceRevision: 'r1' });
      });
      const controller = new ConversationHistoryController(agent(), { request, yieldToBrowser: async () => {}, preparationIntervalMs: 0 });
      try {
        controller.start();
        await vi.waitFor(() => expect(ids(controller)).toEqual(['u1', 'a1']));
        expect(request).toHaveBeenCalledTimes(36);
        expect(controller.state.error).toBeNull();
      } finally {
        controller.cancel();
      }
    });
  });

  describe('bridge stall', () => {
    it('stops a bridge that returns a continuation cursor it has already seen', async () => {
      const request = requestAfterWindow(call => (call === 4 ? bridgeStep(1, [entry('b4', 'assistant', 'x')]) : bridgeStep(call - 1)));
      const controller = await bridgeStops(request);
      try {
        expect(controller.state.error?.code).toBe('stalled');
        expect(controller.state.error?.retryable).toBe(false);
        expect(controller.state.error?.message).toContain('repeated cursor');
        expect(request).toHaveBeenCalledTimes(4);
        expect(ids(controller)).toEqual(windowIds);
        controller.retry();
        expect(request).toHaveBeenCalledTimes(4);
      } finally {
        controller.cancel();
      }
    });

    it('stops a bridge of 32 pages with new cursors and no new entries and offers retry', async () => {
      const request = requestAfterWindow(call => bridgeStep(call, []));
      const controller = await bridgeStops(request);
      try {
        expect(controller.state.error?.code).toBe('stalled');
        expect(controller.state.error?.retryable).toBe(true);
        expect(controller.state.error?.message).toContain('without finding more records');
        expect(request).toHaveBeenCalledTimes(34);
        expect(ids(controller)).toEqual(windowIds);
      } finally {
        controller.cancel();
      }
    });

    it('keeps bridging when new entries arrive after 31 pages without any', async () => {
      const request = requestAfterWindow(call => {
        if (call < 34) return bridgeStep(call, []);
        if (call === 34) return bridgeStep(call);
        if (call === 35) return bridgeStep(call, []);
        return page({ entries: [entry('u2', 'user'), entry('a2', 'assistant')], sourceRevision: 'r1' });
      });
      const controller = await loadWindow(request);
      try {
        controller.refresh();
        await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(36), { timeout: 5000 });
        await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
        expect(controller.state.error).toBeNull();
        expect(ids(controller)).toEqual(['u2', 'a2', 'b34']);
      } finally {
        controller.cancel();
      }
    });
  });

  describe('work deadline', () => {
    const settle = () => vi.advanceTimersByTimeAsync(0);
    const jumpPastDeadline = () => vi.setSystemTime(Date.now() + HISTORY_ACTIVE_DEADLINE_MS);

    async function loadWindowWithFakeTimers(request: ReturnType<typeof requestAfterWindow>) {
      vi.useFakeTimers();
      const controller = newController(request);
      controller.start();
      await settle();
      expect(controller.state.requestPhase).toBe('idle');
      expect(ids(controller)).toEqual(windowIds);
      return controller;
    }

    function expectDeadlineKeptWindow(controller: ConversationHistoryController) {
      expect(controller.state.error?.code).toBe('work_deadline');
      expect(controller.state.error?.retryable).toBe(true);
      expect(ids(controller)).toEqual(windowIds);
      expect(controller.state.nextCursor).toBe('older');
    }

    it('stops between two pages of a bridge and retries from the latest page', async () => {
      const request = requestAfterWindow(call => {
        jumpPastDeadline();
        return bridgeStep(call);
      });
      const controller = await loadWindowWithFakeTimers(request);
      try {
        controller.refresh();
        await settle();
        expectDeadlineKeptWindow(controller);
        expect(request).toHaveBeenCalledTimes(2);
        controller.retry();
        await settle();
        expect(request).toHaveBeenCalledTimes(3);
        expectLatestPageRequested(request, 2);
      } finally {
        controller.cancel();
      }
    });

    it('stops a request that never answers and retries the stored cursor', async () => {
      const request = requestAfterWindow(() => new Promise<ConversationPage>(() => {}));
      const controller = await loadWindowWithFakeTimers(request);
      try {
        controller.demandOlder();
        await settle();
        expect(request).toHaveBeenCalledTimes(2);
        await vi.advanceTimersByTimeAsync(HISTORY_ACTIVE_DEADLINE_MS);
        expectDeadlineKeptWindow(controller);
        controller.retry();
        await settle();
        expect(request).toHaveBeenCalledTimes(3);
        expect(request.mock.calls[2][1]).toMatchObject({ cursor: 'older', retry: true });
      } finally {
        controller.cancel();
      }
    });

    it('stops the wait after a preparing page when the clock is already past the deadline and retries the stored cursor', async () => {
      const request = requestAfterWindow(() => {
        jumpPastDeadline();
        return page({
          state: 'preparing',
          hasMore: true,
          nextCursor: 'older',
          sourceRevision: 'r1',
          progress: { phase: 'scanning', scanned_bytes: 1, source_bytes: 10 },
        });
      });
      const controller = await loadWindowWithFakeTimers(request);
      try {
        controller.demandOlder();
        await settle();
        expectDeadlineKeptWindow(controller);
        expect(request).toHaveBeenCalledTimes(2);
        controller.retry();
        await settle();
        expect(request).toHaveBeenCalledTimes(3);
        expect(request.mock.calls[2][1]).toMatchObject({ cursor: 'older', retry: true });
      } finally {
        controller.cancel();
      }
    });
  });
});

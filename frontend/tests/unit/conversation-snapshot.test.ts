import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { get } from 'svelte/store';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import { clearConversationPreviews, putConversationPreview } from '$lib/conversation-cache';
import { conversationIdentity } from '$lib/conversation-history';
import { securityState, type SecurityState } from '$lib/security';
import { relayStore } from '$lib/store';
import type { Agent, ConversationBatch, ConversationPage } from '$lib/types';

const agent: Agent = {
  relay_id: 'snapshot-test', relay_label: 'Fixture', raw_pane_id: 'pane', pane_id: 'snapshot-test::pane',
  server_session_id: 'primary', terminal_id: 'terminal', generation: 1, agent_session_id: 'session',
  agent: 'pi', cwd: '/fixture', project: 'Snapshot fixture', status: 'working',
};
const entry = (id: string, text: string) => ({ id, role: 'assistant' as const, timestamp: '', text });

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

function savePreview() {
  expect(putConversationPreview({
    identity: conversationIdentity(agent)!, relayId: agent.relay_id, savedAt: Date.now(),
    sourceRevision: 'source:1', historical: true, mode: 'native',
    entries: [entry('cached', 'Cached answer')],
  })).toBe(true);
}

let originalSecurityState: SecurityState;

beforeEach(() => {
  originalSecurityState = get(securityState);
  localStorage.clear();
  localStorage.setItem('herdr-conversation-view', 'activity');
  clearConversationPreviews();
  securityState.set({ ...originalSecurityState, locked: false });
  vi.spyOn(relayStore, 'stopConversationUpdates').mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
  clearConversationPreviews();
  securityState.set(originalSecurityState);
});

it('shows the initial loading state and aborts the real controller request on unmount', async () => {
  const pending = deferred<ConversationPage>();
  const history = vi.spyOn(relayStore, 'getConversationHistory').mockReturnValue(pending.promise);
  const onInitialPage = vi.fn();
  const view = render(ConversationHistory, { agent, onInitialPage });
  expect(screen.getByText('Loading conversation…')).toBeVisible();
  expect(history).toHaveBeenCalledOnce();
  const signal = history.mock.calls[0][1]?.signal;
  expect(signal?.aborted).toBe(false);

  view.unmount();
  expect(signal?.aborted).toBe(true);
  pending.resolve({ available: false, reason: 'Too late', entries: [], hasMore: false, total: 0 });
  await pending.promise;
  await tick();
  expect(onInitialPage).not.toHaveBeenCalled();
});

it('keeps a warm preview until the synchronous initial callback and fresh live observation', async () => {
  savePreview();
  const pending = deferred<ConversationPage>();
  const live = deferred<ConversationBatch>();
  vi.spyOn(relayStore, 'getConversationHistory').mockReturnValue(pending.promise);
  const updates = vi.spyOn(relayStore, 'getConversationUpdates').mockReturnValue(live.promise);
  const callbackOrder: { cached: boolean; fresh: boolean; updates: number }[] = [];
  const onInitialPage = vi.fn((_page: ConversationPage) => {
    callbackOrder.push({ cached: Boolean(screen.queryByText('Cached answer')), fresh: Boolean(screen.queryByText('Fresh answer')), updates: updates.mock.calls.length });
  });
  render(ConversationHistory, { agent, onInitialPage });
  expect(screen.getByText('Cached answer')).toBeVisible();
  expect(screen.getByText('Saved history is shown while current messages are checked.')).toBeVisible();
  expect(screen.getByRole('region', { name: 'Conversation with Snapshot fixture' })).toHaveAttribute('aria-busy', 'true');

  pending.resolve({
    available: true, reason: '', entries: [entry('fresh', 'Fresh answer')], hasMore: false, total: 1,
    state: 'ready', mode: 'native', sourceRevision: 'source:1', subscriptionId: 'sub-1', sequence: 0,
    capabilities: { source: 'pi_events', active_branch: true }, provisional: [entry('live:tail', 'Provisional answer')],
  });
  await screen.findByText('Fresh answer');
  expect(callbackOrder).toEqual([{ cached: true, fresh: false, updates: 0 }]);
  expect(onInitialPage).toHaveBeenCalledOnce();
  expect(onInitialPage.mock.calls[0][0]).toMatchObject({ entries: [entry('fresh', 'Fresh answer')], provisional: undefined });
  expect(screen.queryByText('Cached answer')).not.toBeInTheDocument();
  expect(screen.queryByText('Saved history is shown while current messages are checked.')).not.toBeInTheDocument();
  expect(screen.getByText('Provisional answer')).toBeVisible();
  expect(updates).toHaveBeenCalledOnce();
  expect(updates).toHaveBeenCalledWith(agent, 'source:1', 0, expect.any(AbortSignal), 'sub-1');
  await waitFor(() => expect(screen.getByRole('region', { name: 'Conversation with Snapshot fixture' })).toHaveAttribute('aria-busy', 'false'));
});

it('projects preparation, pause and source errors while preserving the local reload override', async () => {
  savePreview();
  const initial = deferred<ConversationPage>();
  const continued = deferred<ConversationPage>();
  const history = vi.spyOn(relayStore, 'getConversationHistory').mockReturnValueOnce(initial.promise).mockReturnValueOnce(continued.promise);
  const onInitialPage = vi.fn();
  render(ConversationHistory, { agent, onInitialPage });
  initial.resolve({
    available: true, reason: '', entries: [], hasMore: true, nextCursor: 'resume', total: null,
    state: 'preparing', sourceRevision: 'source:1', progress: { phase: 'indexing', scanned_bytes: 40, source_bytes: 100 },
  });
  expect(await screen.findByText(/Preparing history \(indexing\).*40% scanned/)).toBeVisible();
  expect(screen.getByText('Cached answer')).toBeVisible();
  expect(onInitialPage).not.toHaveBeenCalled();
  const region = screen.getByRole('region', { name: 'Conversation with Snapshot fixture' });
  expect(region).toHaveAttribute('aria-busy', 'true');
  await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  expect(screen.getByText('Preparation is paused. Continue when you are ready.')).toBeVisible();
  expect(region).toHaveAttribute('aria-busy', 'false');
  await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  expect(history).toHaveBeenCalledTimes(2);
  expect(history.mock.calls[1][1]).toMatchObject({ cursor: 'resume' });
  expect(region).toHaveAttribute('aria-busy', 'true');

  continued.resolve({
    available: true, reason: '', entries: [], hasMore: true, nextCursor: 'resume', total: null, state: 'failed',
    sourceRevision: 'source:1', error: { code: 'source_changed', message: 'Source changed in fixture.', retryable: false },
  });
  expect(await screen.findByText('Source changed in fixture.')).toBeVisible();
  expect(screen.getByText('Cached answer')).toBeVisible();
  expect(screen.getByRole('button', { name: 'Reload history' })).toBeVisible();
  expect(region).toHaveAttribute('aria-busy', 'false');

  securityState.set({ locked: true } as never);
  await tick();
  await fireEvent.click(screen.getByRole('button', { name: 'Reload history' }));
  expect(screen.queryByRole('button', { name: 'Reload history' })).not.toBeInTheDocument();
  expect(screen.getByText('Source changed in fixture.')).toBeVisible();
  expect(history).toHaveBeenCalledTimes(2);
  expect(onInitialPage).not.toHaveBeenCalled();
});

const capabilities = { source: 'pi_events' as const, active_branch: true };
const page = (subscriptionId: string, text: string): ConversationPage => ({
  available: true, reason: '', entries: [entry('head', text)], hasMore: false, total: 1,
  state: 'ready', sourceRevision: 'source:1', subscriptionId, sequence: 0, capabilities,
});
const batch = (subscriptionId: string, text: string): ConversationBatch => ({
  subscriptionId, kind: 'batch', sourceRevision: 'source:1', sequence: 1,
  entries: [entry('committed', text), entry('live:tail', `${text} provisional`)],
  remove: [], state: 'generating', capabilities,
});

function pauseControl(kind: 'hidden' | 'locked') {
  const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
  return async (paused: boolean) => {
    if (kind === 'hidden') {
      visibility.mockReturnValue(paused ? 'hidden' : 'visible');
      document.dispatchEvent(new Event('visibilitychange'));
    } else {
      securityState.update(state => ({ ...state, locked: paused }));
    }
    await tick();
  };
}

it.each(['hidden', 'locked'] as const)('rejects in-flight initial history after becoming %s and resumes the current target once', async kind => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  const setPaused = pauseControl(kind);
  const oldHead = deferred<ConversationPage>();
  const interruptedHead = deferred<ConversationPage>();
  const resumedHead = deferred<ConversationPage>();
  const live = deferred<ConversationBatch>();
  const history = vi.spyOn(relayStore, 'getConversationHistory')
    .mockReturnValueOnce(oldHead.promise).mockReturnValueOnce(interruptedHead.promise).mockReturnValueOnce(resumedHead.promise);
  const updates = vi.spyOn(relayStore, 'getConversationUpdates').mockReturnValue(live.promise);
  const onInitialPage = vi.fn();
  const next = { ...agent, generation: 2, agent_session_id: 'next-session' };
  const view = render(ConversationHistory, { agent, onInitialPage });
  try {
    expect(history).toHaveBeenCalledOnce();
    const oldSignal = history.mock.calls[0][1]!.signal!;
    await view.rerender({ agent: next });
    expect(oldSignal.aborted).toBe(true);
    expect(history).toHaveBeenCalledTimes(2);
    expect(history.mock.calls[1][0]).toEqual(next);
    const interruptedSignal = history.mock.calls[1][1]!.signal!;
    expect(interruptedSignal.aborted).toBe(false);

    await setPaused(true);
    expect(interruptedSignal.aborted).toBe(true);
    expect(screen.getByText('History paused while the app is hidden or locked.')).toBeVisible();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(history).toHaveBeenCalledTimes(2);
    expect(updates).not.toHaveBeenCalled();

    await setPaused(false);
    expect(history).toHaveBeenCalledTimes(3);
    expect(history.mock.calls[2][0]).toEqual(next);
    expect(history.mock.calls[2][1]).not.toHaveProperty('cursor');
    expect(history.mock.calls[2][1]!.signal!.aborted).toBe(false);
    oldHead.resolve(page('old-sub', 'Old target result'));
    interruptedHead.resolve(page('interrupted-sub', 'Interrupted result'));
    await Promise.all([oldHead.promise, interruptedHead.promise]);
    await tick();
    expect(screen.queryByText('Old target result')).not.toBeInTheDocument();
    expect(screen.queryByText('Interrupted result')).not.toBeInTheDocument();
    expect(onInitialPage).not.toHaveBeenCalled();
    expect(updates).not.toHaveBeenCalled();
    expect(history).toHaveBeenCalledTimes(3);

    resumedHead.resolve(page('current-sub', 'Current target result'));
    expect(await screen.findByText('Current target result')).toBeVisible();
    expect(onInitialPage).toHaveBeenCalledOnce();
    expect(updates).toHaveBeenCalledExactlyOnceWith(next, 'source:1', 0, expect.any(AbortSignal), 'current-sub');
    await vi.advanceTimersByTimeAsync(10_000);
    expect(history).toHaveBeenCalledTimes(3);
    expect(updates).toHaveBeenCalledOnce();
    expect(updates.mock.calls[0][3].aborted).toBe(false);
  } finally {
    view.unmount();
    oldHead.resolve(page('old-sub', 'Old target result'));
    interruptedHead.resolve(page('interrupted-sub', 'Interrupted result'));
    resumedHead.resolve(page('current-sub', 'Current target result'));
    live.resolve(batch('current-sub', 'After unmount'));
  }
});

it.each(['hidden', 'locked'] as const)('stops in-flight older history and live work when %s, then resumes without same-lifetime duplicates', async kind => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  const setPaused = pauseControl(kind);
  const older = deferred<ConversationPage>();
  const refresh = deferred<ConversationPage>();
  const oldLive = deferred<ConversationBatch>();
  const resumedLive = deferred<ConversationBatch>();
  const freshLive = deferred<ConversationBatch>();
  const nextLive = deferred<ConversationBatch>();
  const initialPage = { ...page('initial-sub', 'Recorded head'), hasMore: true, nextCursor: 'older:1', provisional: [entry('live:initial', 'Initial provisional')] };
  const history = vi.spyOn(relayStore, 'getConversationHistory')
    .mockResolvedValueOnce(initialPage).mockReturnValueOnce(older.promise).mockReturnValueOnce(refresh.promise);
  const updates = vi.spyOn(relayStore, 'getConversationUpdates')
    .mockReturnValueOnce(oldLive.promise).mockReturnValueOnce(resumedLive.promise)
    .mockReturnValueOnce(freshLive.promise).mockReturnValueOnce(nextLive.promise);
  const stopped = vi.mocked(relayStore.stopConversationUpdates);
  const view = render(ConversationHistory, { agent });
  let unmounted = false;
  try {
    expect(await screen.findByText('Recorded head')).toBeVisible();
    expect(screen.getByText('Initial provisional')).toBeVisible();
    expect(updates).toHaveBeenCalledExactlyOnceWith(agent, 'source:1', 0, expect.any(AbortSignal), 'initial-sub');
    const oldLiveSignal = updates.mock.calls[0][3];
    await fireEvent.click(screen.getByRole('button', { name: 'Load older turns' }));
    expect(history).toHaveBeenCalledTimes(2);
    expect(history.mock.calls[1][0]).toEqual(agent);
    expect(history.mock.calls[1][1]).toMatchObject({ cursor: 'older:1' });
    const olderSignal = history.mock.calls[1][1]!.signal!;
    expect(olderSignal.aborted).toBe(false);
    expect(oldLiveSignal.aborted).toBe(false);

    await setPaused(true);
    expect(olderSignal.aborted).toBe(true);
    expect(oldLiveSignal.aborted).toBe(true);
    expect(stopped).toHaveBeenCalledExactlyOnceWith(agent, 'initial-sub');
    expect(screen.getByText('Updates paused.')).toBeVisible();
    expect(screen.getByText('History paused while the app is hidden or locked.')).toBeVisible();
    expect(screen.getByText('Recorded head')).toBeVisible();
    expect(screen.queryByText('Initial provisional')).not.toBeInTheDocument();
    older.resolve({ ...page('stale-history-sub', 'Late older history'), hasMore: false });
    oldLive.resolve(batch('initial-sub', 'Late old live'));
    await Promise.all([older.promise, oldLive.promise]);
    await tick();
    expect(screen.queryByText('Late older history')).not.toBeInTheDocument();
    expect(screen.queryByText('Late old live')).not.toBeInTheDocument();
    expect(screen.queryByText('Late old live provisional')).not.toBeInTheDocument();
    await fireEvent.click(screen.getByRole('button', { name: 'Load older turns' }));
    await vi.advanceTimersByTimeAsync(10_000);
    expect(history).toHaveBeenCalledTimes(2);
    expect(updates).toHaveBeenCalledOnce();
    expect(stopped).toHaveBeenCalledOnce();

    await setPaused(false);
    // The old head can be re-observed during resume before the refresh returns.
    expect(history).toHaveBeenCalledTimes(3);
    expect(history.mock.calls[2][0]).toEqual(agent);
    expect(history.mock.calls[2][1]).not.toHaveProperty('cursor');
    expect(history.mock.calls[2][1]!.signal!.aborted).toBe(false);
    expect(updates).toHaveBeenCalledTimes(2);
    expect(updates.mock.calls[1]).toEqual([agent, 'source:1', 0, expect.any(AbortSignal), 'initial-sub']);
    const resumedLiveSignal = updates.mock.calls[1][3];
    expect(resumedLiveSignal).not.toBe(oldLiveSignal);
    expect(resumedLiveSignal.aborted).toBe(false);
    expect(stopped).toHaveBeenCalledOnce();

    refresh.resolve({ ...page('fresh-sub', 'Recorded head'), hasMore: true, nextCursor: 'older:1', provisional: [entry('live:fresh', 'Fresh provisional')] });
    expect(await screen.findByText('Fresh provisional')).toBeVisible();
    expect(resumedLiveSignal.aborted).toBe(true);
    expect(updates).toHaveBeenCalledTimes(3);
    expect(updates.mock.calls[2]).toEqual([agent, 'source:1', 0, expect.any(AbortSignal), 'fresh-sub']);
    const freshSignal = updates.mock.calls[2][3];
    expect(freshSignal.aborted).toBe(false);
    resumedLive.resolve(batch('initial-sub', 'Late resumed live'));
    await resumedLive.promise;
    await tick();
    expect(screen.queryByText('Late resumed live')).not.toBeInTheDocument();
    expect(screen.queryByText('Late resumed live provisional')).not.toBeInTheDocument();
    expect(updates).toHaveBeenCalledTimes(3);

    freshLive.resolve(batch('fresh-sub', 'Current committed'));
    expect(await screen.findByText('Current committed')).toBeVisible();
    expect(screen.getByText('Current committed provisional')).toBeVisible();
    expect(updates).toHaveBeenCalledTimes(4);
    expect(updates.mock.calls[3]).toEqual([agent, 'source:1', 1, freshSignal, 'fresh-sub']);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(history).toHaveBeenCalledTimes(3);
    expect(updates).toHaveBeenCalledTimes(4);
    expect(stopped).toHaveBeenCalledOnce();
    view.unmount();
    unmounted = true;
    expect(freshSignal.aborted).toBe(true);
    expect(stopped).toHaveBeenCalledTimes(2);
    expect(stopped).toHaveBeenLastCalledWith(agent, 'fresh-sub');
  } finally {
    if (!unmounted) view.unmount();
    older.resolve(page('stale-history-sub', 'Late older history'));
    refresh.resolve(page('fresh-sub', 'Recorded head'));
    oldLive.resolve(batch('initial-sub', 'Late old live'));
    resumedLive.resolve(batch('initial-sub', 'Late resumed live'));
    freshLive.resolve(batch('fresh-sub', 'Current committed'));
    nextLive.resolve(batch('fresh-sub', 'After unmount'));
  }
});

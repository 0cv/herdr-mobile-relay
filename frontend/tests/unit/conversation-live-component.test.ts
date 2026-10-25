import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import { clearConversationPreviews } from '$lib/conversation-cache';
import { conversationIdentity } from '$lib/conversation-history';
import { targetRefForAgent } from '$lib/resource-id';
import { relayStore } from '$lib/store';
import type { Agent, ConversationBatch, ConversationPage } from '$lib/types';

const agent: Agent = { relay_id: 'live-fixture', relay_label: 'Fixture', raw_pane_id: 'pane', pane_id: 'live-fixture::pane', server_session_id: 'primary', terminal_id: 'terminal', generation: 1, agent_session_id: 'session', agent: 'pi', cwd: '/fixture', status: 'working' };
const capabilities = { source: 'pi_events' as const, active_branch: true };
const liveLabel = 'Live events · provisional text';
const unavailableLabel = 'Live events unavailable — recorded history only.';
const entry = (id: string, text = id) => ({ id, role: 'assistant' as const, timestamp: '', text });
const livePage: ConversationPage = { available: true, reason: '', entries: [entry('pi:answer')], hasMore: false, total: 1, state: 'ready', sourceRevision: 'source:1', subscriptionId: 'sub-1', sequence: 0, capabilities, provisional: [entry('live:old', 'Old provisional text')] };
const unavailablePage: ConversationPage = { available: false, reason: 'Fixture history unavailable', entries: [], hasMore: false, total: 0, state: 'ready' };

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

beforeEach(() => {
  clearConversationPreviews();
  localStorage.setItem('herdr-conversation-view', 'activity');
  vi.spyOn(relayStore, 'stopConversationUpdates').mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  clearConversationPreviews();
});

describe.each([
  ['provider', { agent: undefined }],
  ['session', { agent_session_id: undefined }],
] satisfies [string, Partial<Agent>][])('conversation live targets with missing %s metadata', (_name, missing) => {
  const initial = { ...agent, ...missing };
  const next = { ...initial, raw_pane_id: 'other-pane', pane_id: 'live-fixture::other-pane', terminal_id: 'other-terminal', generation: 2 };

  it('clears live attribution during a delayed, then unavailable target switch and ignores stale batches', async () => {
    expect(conversationIdentity(initial)).toBeNull();
    expect(conversationIdentity(next)).toBeNull();
    expect(targetRefForAgent(initial)).not.toBeNull();
    expect(targetRefForAgent(next)).not.toEqual(targetRefForAgent(initial));
    const delayed = deferred<ConversationPage>();
    const oldBatch = deferred<ConversationBatch>();
    const history = vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValueOnce(structuredClone(livePage)).mockReturnValueOnce(delayed.promise);
    const updates = vi.spyOn(relayStore, 'getConversationUpdates').mockReturnValue(oldBatch.promise);
    const view = render(ConversationHistory, { agent: initial });
    await waitFor(() => expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument());
    expect(screen.getByText('Old provisional text')).toBeVisible();
    const signal = updates.mock.calls[0][3];

    await view.rerender({ agent: { ...initial, status: 'idle', project: 'Renamed fixture' } });
    expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument();
    expect(screen.getByText('Old provisional text')).toBeVisible();
    expect(history).toHaveBeenCalledOnce();
    expect(updates).toHaveBeenCalledOnce();
    expect(signal?.aborted).toBe(false);
    expect(relayStore.stopConversationUpdates).not.toHaveBeenCalled();

    await view.rerender({ agent: next });
    expect(screen.getByText(unavailableLabel)).toBeVisible();
    expect(screen.queryByText(liveLabel)).not.toBeInTheDocument();
    expect(screen.queryByText('Old provisional text')).not.toBeInTheDocument();
    expect(screen.queryByText('pi:answer')).not.toBeInTheDocument();
    expect(signal?.aborted).toBe(true);
    expect(relayStore.stopConversationUpdates).toHaveBeenCalledWith(initial, 'sub-1');
    expect(history).toHaveBeenLastCalledWith(next, expect.anything());

    oldBatch.resolve({ subscriptionId: 'sub-1', kind: 'batch', sourceRevision: 'source:1', sequence: 1, entries: [entry('live:late', 'Late old output'), entry('pi:late', 'Late old committed text')], remove: [], state: 'generating', capabilities });
    await oldBatch.promise;
    await tick();
    expect(screen.queryByText('Late old output')).not.toBeInTheDocument();
    expect(screen.queryByText('Late old committed text')).not.toBeInTheDocument();
    expect(screen.getByText(unavailableLabel)).toBeVisible();
    expect(updates).toHaveBeenCalledOnce();

    delayed.resolve(unavailablePage);
    await screen.findByText('Fixture history unavailable');
    expect(screen.getByText(unavailableLabel)).toBeVisible();
    expect(updates).toHaveBeenCalledOnce();
  });

  it('recovers from a fresh current-target snapshot without accepting an old-target error', async () => {
    const delayed = deferred<ConversationPage>();
    const oldBatch = deferred<ConversationBatch>();
    const currentBatch = deferred<ConversationBatch>();
    const pending = deferred<ConversationBatch>();
    const history = vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValueOnce(structuredClone(livePage)).mockReturnValueOnce(delayed.promise);
    const updates = vi.spyOn(relayStore, 'getConversationUpdates').mockReturnValueOnce(oldBatch.promise).mockReturnValueOnce(currentBatch.promise).mockReturnValue(pending.promise);
    const view = render(ConversationHistory, { agent: initial });
    await waitFor(() => expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument());
    const signal = updates.mock.calls[0][3];
    await view.rerender({ agent: next });
    expect(screen.getByText(unavailableLabel)).toBeVisible();
    expect(signal?.aborted).toBe(true);

    delayed.resolve({ ...structuredClone(livePage), subscriptionId: 'sub-current', entries: [entry('pi:current', 'Current recorded history')], provisional: [] });
    await screen.findByText('Current recorded history');
    expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument();
    expect(screen.queryByText('Old provisional text')).not.toBeInTheDocument();
    expect(updates).toHaveBeenLastCalledWith(next, 'source:1', 0, expect.any(AbortSignal), 'sub-current');

    oldBatch.reject(new Error('Old target disconnected'));
    await oldBatch.promise.catch(() => {});
    await tick();
    expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument();
    expect(screen.queryByText('Disconnected — loaded history remains readable.')).not.toBeInTheDocument();
    expect(history).toHaveBeenCalledTimes(2);

    currentBatch.resolve({ subscriptionId: 'sub-current', kind: 'batch', sourceRevision: 'source:1', sequence: 1, entries: [entry('live:current', 'Current provisional text')], remove: [], state: 'generating', capabilities });
    await screen.findByText('Current provisional text');
    expect(screen.getByText('Current recorded history')).toBeVisible();
    expect(updates).toHaveBeenCalledTimes(3);
  });

  it('does not rebind current updates to a superseded history snapshot with the same source revision', async () => {
    const delayed = deferred<ConversationPage>();
    const oldBatch = deferred<ConversationBatch>();
    const currentBatch = deferred<ConversationBatch>();
    const pending = deferred<ConversationBatch>();
    const current = { ...next, generation: 3 };
    vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValueOnce(structuredClone(livePage)).mockReturnValueOnce(delayed.promise).mockResolvedValueOnce({ ...structuredClone(livePage), subscriptionId: 'sub-current', entries: [entry('pi:current', 'Current recorded history')], provisional: [] });
    const updates = vi.spyOn(relayStore, 'getConversationUpdates').mockReturnValueOnce(oldBatch.promise).mockReturnValueOnce(currentBatch.promise).mockReturnValue(pending.promise);
    const view = render(ConversationHistory, { agent: initial });
    await waitFor(() => expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument());
    await view.rerender({ agent: next });
    await view.rerender({ agent: current });
    await screen.findByText('Current recorded history');

    delayed.resolve({ ...structuredClone(livePage), subscriptionId: 'sub-stale', entries: [entry('pi:late', 'Late old history')] });
    await delayed.promise;
    await tick();
    currentBatch.resolve({ subscriptionId: 'sub-current', kind: 'batch', sourceRevision: 'source:1', sequence: 1, entries: [entry('pi:current-update', 'Current committed update')], remove: [], state: 'generating', capabilities });
    await screen.findByText('Current committed update');
    expect(screen.getByText('Current recorded history')).toBeVisible();
    expect(screen.queryByText('Late old history')).not.toBeInTheDocument();
    expect(screen.queryByText('Old provisional text')).not.toBeInTheDocument();
    expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument();
    expect(updates).toHaveBeenCalledTimes(3);
    expect(updates).toHaveBeenLastCalledWith(current, 'source:1', 1, expect.any(AbortSignal), 'sub-current');
  });

  it('aborts superseded history requests and ignores their delayed live snapshots', async () => {
    const delayed = deferred<ConversationPage>();
    const pending = deferred<ConversationBatch>();
    const history = vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValueOnce(structuredClone(livePage)).mockReturnValueOnce(delayed.promise).mockResolvedValueOnce(unavailablePage);
    const updates = vi.spyOn(relayStore, 'getConversationUpdates').mockReturnValue(pending.promise);
    const view = render(ConversationHistory, { agent: initial });
    await waitFor(() => expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument());
    await view.rerender({ agent: next });
    const signal = history.mock.calls[1][1]?.signal;
    await view.rerender({ agent: { ...next, generation: 3 } });
    await screen.findByText('Fixture history unavailable');
    expect(signal?.aborted).toBe(true);
    expect(updates.mock.calls[0][3]?.aborted).toBe(true);
    delayed.resolve({ ...structuredClone(livePage), subscriptionId: 'sub-stale', entries: [entry('pi:late', 'Late old history')] });
    await delayed.promise;
    await tick();
    expect(screen.getByText(unavailableLabel)).toBeVisible();
    expect(screen.queryByText('Late old history')).not.toBeInTheDocument();
    expect(screen.queryByText('Old provisional text')).not.toBeInTheDocument();
    expect(updates).toHaveBeenCalledOnce();
  });
});

describe('conversation live target changes', () => {
  it.each([
    ['pane', { raw_pane_id: 'other-pane', pane_id: 'live-fixture::other-pane', terminal_id: 'other-terminal' }],
    ['generation', { generation: 2 }],
    ['session', { agent_session_id: 'other-session' }],
    ['provider', { agent: 'claude' }],
    ['directory', { cwd: '/other-fixture' }],
  ] satisfies [string, Partial<Agent>][])('clears live attribution during a delayed, then unavailable %s change', async (_name, change) => {
    const next = { ...agent, ...change };
    const delayed = deferred<ConversationPage>();
    const oldBatch = deferred<ConversationBatch>();
    const history = vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValueOnce(structuredClone(livePage)).mockReturnValueOnce(delayed.promise);
    const updates = vi.spyOn(relayStore, 'getConversationUpdates').mockReturnValue(oldBatch.promise);
    const view = render(ConversationHistory, { agent });
    await waitFor(() => expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument());
    expect(screen.getByText('Old provisional text')).toBeVisible();
    const signal = updates.mock.calls[0][3];

    await view.rerender({ agent: next });
    expect(screen.getByText(unavailableLabel)).toBeVisible();
    expect(screen.queryByText(liveLabel)).not.toBeInTheDocument();
    expect(screen.queryByText('Old provisional text')).not.toBeInTheDocument();
    expect(signal?.aborted).toBe(true);
    expect(relayStore.stopConversationUpdates).toHaveBeenCalledWith(agent, 'sub-1');
    expect(history).toHaveBeenLastCalledWith(next, expect.anything());

    oldBatch.resolve({ subscriptionId: 'sub-1', kind: 'batch', sourceRevision: 'source:1', sequence: 1, entries: [entry('live:late', 'Late old output')], remove: [], state: 'generating', capabilities });
    await oldBatch.promise;
    await tick();
    expect(screen.queryByText('Late old output')).not.toBeInTheDocument();
    expect(screen.getByText(unavailableLabel)).toBeVisible();
    expect(updates).toHaveBeenCalledOnce();

    delayed.resolve(unavailablePage);
    await screen.findByText('Fixture history unavailable');
    expect(screen.getByText(unavailableLabel)).toBeVisible();
    expect(updates).toHaveBeenCalledOnce();
  });

  it('ignores late old-target failures and restores live updates only from a current-target snapshot', async () => {
    const next = { ...agent, generation: 2 };
    const oldBatch = deferred<ConversationBatch>();
    const currentBatch = deferred<ConversationBatch>();
    const currentPending = deferred<ConversationBatch>();
    const history = vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValueOnce(structuredClone(livePage)).mockResolvedValueOnce(unavailablePage);
    const updates = vi.spyOn(relayStore, 'getConversationUpdates').mockReturnValueOnce(oldBatch.promise).mockReturnValueOnce(currentBatch.promise).mockReturnValue(currentPending.promise);
    const view = render(ConversationHistory, { agent });
    await waitFor(() => expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument());
    await view.rerender({ agent: next });
    await screen.findByText('Fixture history unavailable');

    oldBatch.reject(new Error('Old target disconnected'));
    await oldBatch.promise.catch(() => {});
    await tick();
    expect(screen.getByText(unavailableLabel)).toBeVisible();
    expect(screen.queryByText('Disconnected — loaded history remains readable.')).not.toBeInTheDocument();

    history.mockResolvedValueOnce({ ...structuredClone(livePage), subscriptionId: 'sub-current', provisional: [] });
    await view.rerender({ agent });
    await waitFor(() => expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument());
    currentBatch.resolve({ subscriptionId: 'sub-current', kind: 'batch', sourceRevision: 'source:1', sequence: 1, entries: [entry('live:current', 'Current provisional text')], remove: [], state: 'generating', capabilities });
    await screen.findByText('Current provisional text');
    expect(screen.queryByText('Old provisional text')).not.toBeInTheDocument();
    expect(updates).toHaveBeenCalledTimes(3);
  });

  it('does not rebind live updates for status and title churn on the same target', async () => {
    const pending = deferred<ConversationBatch>();
    const history = vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue(structuredClone(livePage));
    const updates = vi.spyOn(relayStore, 'getConversationUpdates').mockReturnValue(pending.promise);
    const view = render(ConversationHistory, { agent });
    await waitFor(() => expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument());
    await view.rerender({ agent: { ...agent, status: 'idle', project: 'Renamed fixture' } });
    expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument();
    expect(screen.getByText('Old provisional text')).toBeVisible();
    expect(history).toHaveBeenCalledOnce();
    expect(updates).toHaveBeenCalledOnce();
    expect(relayStore.stopConversationUpdates).not.toHaveBeenCalled();
  });

  it('ignores a delayed history response after switching again to an unavailable target', async () => {
    const delayed = deferred<ConversationPage>();
    const pending = deferred<ConversationBatch>();
    vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValueOnce(structuredClone(livePage)).mockReturnValueOnce(delayed.promise).mockResolvedValueOnce(unavailablePage);
    const updates = vi.spyOn(relayStore, 'getConversationUpdates').mockReturnValue(pending.promise);
    const view = render(ConversationHistory, { agent });
    await waitFor(() => expect(document.querySelector('.conversation-source-status')).not.toBeInTheDocument());
    await view.rerender({ agent: { ...agent, generation: 2 } });
    await view.rerender({ agent: { ...agent, generation: 3 } });
    await screen.findByText('Fixture history unavailable');
    delayed.resolve({ ...structuredClone(livePage), subscriptionId: 'sub-stale', entries: [entry('pi:late', 'Late old history')] });
    await delayed.promise;
    await tick();
    await waitFor(() => expect(screen.getByText(unavailableLabel)).toBeVisible());
    expect(screen.queryByText('Late old history')).not.toBeInTheDocument();
    expect(updates).toHaveBeenCalledOnce();
  });
});

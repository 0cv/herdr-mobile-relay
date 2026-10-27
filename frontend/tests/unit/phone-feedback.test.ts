import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import PromptComposer from '$components/PromptComposer.svelte';
import ConversationHistory from '$components/ConversationHistory.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { relayStore } from '$lib/store';
import { clearPromptDraft, loadPromptDraft } from '$lib/prompt-drafts';
import { targetRefForAgent } from '$lib/resource-id';
import type { Agent, ConversationBatch, ConversationCapabilities, RelayConnectionView, TerminalFrame } from '$lib/types';

const agent: Agent = { relay_id: 'phone-feedback', relay_label: 'Fixture', pane_id: 'phone-feedback::pane', raw_pane_id: 'pane', server_session_id: 'server', terminal_id: 'terminal', generation: 1, agent_session_id: 'session', agent: 'pi', status: 'idle', cwd: '/fixture' };
const healthy = { status: 'connected', capabilities: [], inventory: { state: 'ready', stale: false } } as unknown as RelayConnectionView;
const frame = (): TerminalFrame => ({ paneId: agent.pane_id, target: targetRefForAgent(agent), content: 'Ready', noEcho: false, receivedAt: performance.now(), format: 'plain' });
beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear();
  clearPromptDraft(agent);
  relayStore.connections.set(new Map([[agent.relay_id, healthy]]) as never);
  for (const method of ['readPane', 'watchPane', 'unwatchPane', 'stopConversationUpdates', 'showToast'] as const) vi.spyOn(relayStore, method).mockImplementation(() => {});
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({ available: true, entries: [], hasMore: false, total: 0, reason: '', state: 'ready' });
});
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); localStorage.clear(); clearPromptDraft(agent); relayStore.connections.set(new Map()); });

it.each(['idle', 'working', 'unknown', 'done'])('omits the activity footer and announcer for %s agents', status => {
  render(PromptComposer, { agent: { ...agent, status } });
  expect(document.querySelector('.agent-progress, [role="status"][aria-atomic="true"]')).toBeNull();
  expect(screen.queryByText(/Working…|Waiting for reply|Finished|Agent status unknown/)).not.toBeInTheDocument();
});

it.each([TerminalView, ConversationHistory])('shows no empty-composer draft warning and keeps unsaved text neutral without weakening fresh-frame gates', async component => {
  const view = render(component as typeof TerminalView, { agent, allAgents: [agent], responding: new Set<string>() });
  await vi.advanceTimersByTimeAsync(0);
  expect(document.querySelector('.draft-status')).toBeNull();
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'synthetic unsaved text' } });
  const status = screen.getByText(/Not saved on this phone/);
  expect(status).toHaveClass('draft-status');
  expect(status).not.toHaveClass('error');
  expect(loadPromptDraft(agent)).toBe('');
  await view.rerender({ frame: frame() });
  expect(loadPromptDraft(agent)).toBe('');
  expect(status).toBeVisible();
  await fireEvent.click(screen.getByRole('button', { name: 'Clear prompt text' }));
  expect(document.querySelector('.draft-status')).toBeNull();
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'safe boundary draft' } });
  expect(loadPromptDraft(agent)).toBe('safe boundary draft');
  expect(document.querySelector('.draft-status')).toBeNull();
});

it('does not reclassify actual draft storage failures as a healthy or neutral state', async () => {
  render(PromptComposer, { agent, draftPersistenceAllowed: true });
  vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('Synthetic storage failure'); });
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'safe but not persisted' } });
  expect(screen.getByText('Draft persistence is unavailable; keep this page open.')).toHaveClass('error');
});

it.each([
  { source: 'pi_events', active_branch: true },
  { source: 'recorded_history', active_branch: false },
  { source: 'recorded_history', active_branch: true },
  { source: 'recorded_history' },
] as ConversationCapabilities[])('removes the healthy banner but preserves branch trust for %j', async capabilities => {
  localStorage.setItem('herdr-conversation-view', 'activity');
  const { source } = capabilities;
  vi.mocked(relayStore.getConversationHistory).mockResolvedValue({ available: true, reason: '', entries: [{ id: 'saved', role: 'assistant', text: 'Recorded answer', timestamp: '' }], hasMore: false, total: 1, state: 'ready', sourceRevision: 'revision', subscriptionId: 'subscription', capabilities, provisional: source === 'pi_events' ? [{ id: 'live:partial', role: 'assistant', text: 'Provisional answer', timestamp: '' }] : [] });
  vi.spyOn(relayStore, 'getConversationUpdates').mockImplementation(() => new Promise<ConversationBatch>(() => {}));
  render(ConversationHistory, { agent });
  await vi.advanceTimersByTimeAsync(0);
  expect(screen.getByText('Recorded answer')).toBeVisible();
  expect(document.querySelector('.conversation-page > .conversation-source-status')).toBeNull();
  const trust = document.querySelector('.conversation-toolbar .conversation-branch-trust');
  if (source === 'recorded_history' && capabilities.active_branch !== true) {
    expect(trust).toBeVisible();
    expect(trust).toHaveTextContent('Branch unverified');
    expect(trust).toHaveTextContent('saved history may not reflect the current branch');
    expect(trust?.querySelector('.sr-only')).toHaveTextContent('saved history may not reflect the current branch');
  } else {
    expect(trust).toBeNull();
  }
  const saved = document.querySelector('[data-conversation-entry-id="saved"]');
  expect(saved).not.toHaveTextContent('not saved yet');
  if (source === 'pi_events') {
    expect(document.querySelector('[data-conversation-entry-id="live:partial"]')).toHaveTextContent('Live · not saved yet');
  }
});

it('clears the old branch caveat before loading another target and accepts fresh validated trust', async () => {
  localStorage.setItem('herdr-conversation-view', 'activity');
  const page = { available: true, reason: '', entries: [{ id: 'saved', role: 'assistant' as const, text: 'Recorded answer', timestamp: '' }], hasMore: false, total: 1, state: 'ready' as const, sourceRevision: 'revision', subscriptionId: 'subscription', capabilities: { source: 'recorded_history' as const, active_branch: false } };
  const pageWithValidatedBranch = { ...page, capabilities: { ...page.capabilities, active_branch: true } };
  let resolve!: (page: typeof pageWithValidatedBranch) => void;
  vi.mocked(relayStore.getConversationHistory).mockResolvedValueOnce(page).mockImplementationOnce(() => new Promise(yes => { resolve = yes; })).mockResolvedValue({ ...page, capabilities: { source: 'pi_events', active_branch: true }, provisional: [{ id: 'live:new', role: 'assistant', text: 'New live answer', timestamp: '' }] });
  vi.spyOn(relayStore, 'getConversationUpdates').mockImplementation(() => new Promise<ConversationBatch>(() => {}));
  const view = render(ConversationHistory, { agent });
  await vi.advanceTimersByTimeAsync(0);
  expect(document.querySelector('.conversation-branch-trust')).toBeVisible();
  await view.rerender({ agent: { ...agent, generation: 2 } });
  expect(document.querySelector('.conversation-branch-trust')).toBeNull();
  expect(screen.getByText('Loading conversation…')).toBeVisible();
  resolve(pageWithValidatedBranch);
  await vi.advanceTimersByTimeAsync(0);
  expect(screen.getByText('Recorded answer')).toBeVisible();
  expect(document.querySelector('.conversation-branch-trust')).toBeNull();
  expect(document.querySelector('.conversation-page > .conversation-source-status')).toBeNull();
  await view.rerender({ agent: { ...agent, generation: 3 } });
  await vi.advanceTimersByTimeAsync(0);
  expect(document.querySelector('.conversation-branch-trust')).toBeNull();
  expect(document.querySelector('[data-conversation-entry-id="live:new"]')).toHaveTextContent('Live · not saved yet');
  expect(document.querySelector('.conversation-page > .conversation-source-status')).toBeNull();
});

it('retains live incomplete and disconnection feedback', async () => {
  const capabilities = { source: 'pi_events' as const, active_branch: true };
  vi.mocked(relayStore.getConversationHistory).mockResolvedValue({ available: true, reason: '', entries: [], hasMore: false, total: 0, state: 'ready', liveState: 'incomplete', sourceRevision: 'revision', subscriptionId: 'subscription', capabilities });
  let reject!: (error: Error) => void;
  vi.spyOn(relayStore, 'getConversationUpdates').mockImplementation(() => new Promise((_yes, no) => { reject = no; }));
  render(ConversationHistory, { agent });
  await vi.advanceTimersByTimeAsync(0);
  expect(screen.getByText('Live output is incomplete — use Terminal for current activity.')).toBeVisible();
  reject(new Error('Synthetic disconnect'));
  await vi.advanceTimersByTimeAsync(0);
  expect(screen.getByText('Disconnected — loaded history remains readable.')).toBeVisible();
});

import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import { conversationEntries } from '$lib/conversation';
import { clearConversationPreviews } from '$lib/conversation-cache';
import { ConversationHistoryController } from '$lib/conversation-history';
import { normalizeConversationBatch, normalizeConversationPage, relayStore } from '$lib/store';
import { targetRefForAgent } from '$lib/resource-id';
import type { Agent, ConversationBatch, ConversationEntry, ConversationPage } from '$lib/types';

const agent: Agent = { relay_id: 'notices', relay_label: 'Fixture', raw_pane_id: 'pane', pane_id: 'notices::pane', server_session_id: 'server', terminal_id: 'terminal', generation: 1, agent_session_id: 'session', agent: 'pi', status: 'blocked', attention_kind: 'unknown', project: 'Pi' };
const notice: ConversationEntry = { id: 'pi:notice', role: 'assistant', kind: 'notice', text: '[orchestration-information-request] Answer with /orchestrate-answer synthetic-run <guidance>.', timestamp: '' };
const entries: ConversationEntry[] = [{ id: 'pi:user', role: 'user', text: 'Start a workflow', timestamp: '' }, { id: 'pi:answer', role: 'assistant', text: 'Preceding answer', timestamp: '' }, notice];
const page = (rows = entries, extra: Partial<ConversationPage> = {}): ConversationPage => ({ available: true, reason: '', entries: rows, hasMore: false, total: rows.length, sourceRevision: 'revision', state: 'ready', ...extra });

beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear();
  clearConversationPreviews();
  relayStore.connections.set(new Map([[agent.relay_id, { status: 'connected', capabilities: [], inventory: { state: 'ready', stale: false } }]] as never));
  vi.spyOn(relayStore, 'readPane').mockImplementation(() => {});
  vi.spyOn(relayStore, 'stopConversationUpdates').mockImplementation(() => {});
  vi.spyOn(relayStore, 'showToast').mockImplementation(() => {});
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue(page());
  vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({ type: 'command_result', request_id: 'sent', ok: true });
});
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); localStorage.clear(); clearConversationPreviews(); relayStore.connections.set(new Map()); });

it('preserves every notice and the preceding answer without mutating Full history', () => {
  const toolAnswer = { ...entries[1], tools: [{ name: 'read', input: 'fixture' }] };
  const later = { ...entries[1], id: 'later', text: 'Later answer' };
  const secondNotice = { ...notice, id: 'pi:second', text: 'A generic extension notice' };
  const recorded = [entries[0], toolAnswer, notice, secondNotice, later];
  expect(conversationEntries(recorded)).toEqual([...entries.slice(0, 2), notice, secondNotice, later]);
  expect(toolAnswer.tools).toHaveLength(1);
  expect(recorded[2]).toBe(notice);
});

it('preserves notice kind in page and live wire normalization but drops metadata', () => {
  const raw = { ...notice, details: 'PRIVATE_DETAILS' };
  const normalized = normalizeConversationPage({ available: true, entries: [raw], has_more: false, state: 'ready', mode: 'native', source_revision: 'revision', total: 1 });
  expect(normalized.entries[0]).toMatchObject(notice);
  const batch = normalizeConversationBatch({ subscription_id: 'subscription', kind: 'batch', source_revision: 'revision', sequence: 1, entries: [raw], remove: [], state: 'ready', capabilities: { source: 'pi_events', active_branch: true } });
  expect(batch.entries[0]).toMatchObject(notice);
  expect(JSON.stringify([normalized, batch])).not.toContain('PRIVATE_');
});

it.each(['conversation', 'activity'])('shows generic notices without unlocking unknown input focus in %s', async mode => {
  localStorage.setItem('herdr-conversation-view', mode);
  render(ConversationHistory, { agent, frame: { paneId: agent.pane_id, target: targetRefForAgent(agent), content: '> ', noEcho: false, receivedAt: performance.now(), format: 'plain' } });
  await vi.advanceTimersByTimeAsync(0);
  const article = document.querySelector('[data-conversation-entry-id="pi:notice"]');
  expect(article).toHaveClass('conversation-notice');
  expect(article).toHaveTextContent('Extension notice');
  expect(article).toHaveTextContent('/orchestrate-answer synthetic-run');
  expect(screen.getByText('Preceding answer')).toBeVisible();
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: '/orchestrate-answer synthetic-run guidance' } });
  expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
  await fireEvent.keyDown(screen.getByLabelText('Prompt'), { key: 'Enter', ctrlKey: true });
  expect(relayStore.sendToAgent).not.toHaveBeenCalled();
});

it.each(['conversation', 'activity'])('renders live notice upserts once and preserves active-branch trust in %s', async mode => {
  localStorage.setItem('herdr-conversation-view', mode);
  const capabilities = { source: 'pi_events' as const, active_branch: true };
  vi.mocked(relayStore.getConversationHistory).mockResolvedValue(page(entries.slice(0, 2), { subscriptionId: 'subscription', sequence: 0, capabilities }));
  let deliver!: (batch: ConversationBatch) => void;
  vi.spyOn(relayStore, 'getConversationUpdates').mockImplementation(() => new Promise(resolve => { deliver = resolve; }));
  const view = render(ConversationHistory, { agent });
  await vi.advanceTimersByTimeAsync(0);
  deliver({ subscriptionId: 'subscription', kind: 'batch', sourceRevision: 'revision', sequence: 1, entries: [notice], remove: [], state: 'ready', capabilities });
  await vi.advanceTimersByTimeAsync(0);
  deliver({ subscriptionId: 'subscription', kind: 'batch', sourceRevision: 'revision', sequence: 2, entries: [notice], remove: [], state: 'ready', capabilities });
  await vi.advanceTimersByTimeAsync(0);
  await view.rerender({ agent: { ...agent, project: 'Renamed' } });
  expect(document.querySelectorAll('[data-conversation-entry-id="pi:notice"]')).toHaveLength(1);
  expect(screen.getByText('Extension notice')).toBeVisible();
  expect(document.querySelector('.conversation-branch-trust')).toBeNull();
});

it.each(['conversation', 'activity'])('keeps a leading notice visible before an incomplete exchange in %s', async mode => {
  localStorage.setItem('herdr-conversation-view', mode);
  vi.mocked(relayStore.getConversationHistory).mockResolvedValue(page([notice, ...entries.slice(0, 2)], { hasMore: true, nextCursor: 'older' }));
  render(ConversationHistory, { agent });
  await vi.advanceTimersByTimeAsync(0);
  expect(screen.getByText('Extension notice')).toBeVisible();
  expect(relayStore.getConversationHistory).toHaveBeenCalledTimes(1);
});

it('merges older paging overlap and live notices by ID without duplicates', async () => {
  const older = { ...notice, id: 'pi:older', text: 'Older notice' };
  let finish!: (value: ConversationPage) => void;
  const request = vi.fn().mockResolvedValueOnce(page([notice], { hasMore: true, nextCursor: 'older' }))
    .mockImplementationOnce(() => new Promise<ConversationPage>(resolve => { finish = resolve; }));
  const controller = new ConversationHistoryController(agent, { request });
  try {
    controller.start();
    await vi.advanceTimersByTimeAsync(0);
    controller.demandOlder();
    expect(controller.applyLiveEntries('revision', [{ ...notice, text: 'Updated notice' }], 1)).toBe(true);
    finish(page([older, notice]));
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.state.entries.map(entry => entry.id)).toEqual(['pi:older', 'pi:notice']);
    expect(controller.state.entries[1]).toMatchObject({ kind: 'notice', text: 'Updated notice' });
  } finally { controller.cancel(); }
});

it('keeps recorded branch caveats and the existing prompt-ready slash submission path', async () => {
  vi.mocked(relayStore.getConversationHistory).mockResolvedValue(page(entries, { subscriptionId: 'subscription', capabilities: { source: 'recorded_history', active_branch: false } }));
  vi.spyOn(relayStore, 'getConversationUpdates').mockImplementation(() => new Promise(() => {}));
  const current = { ...agent, attention_kind: 'chat' as const };
  render(ConversationHistory, { agent: current });
  await vi.advanceTimersByTimeAsync(0);
  expect(document.querySelector('.conversation-branch-trust')).toHaveTextContent('Branch unverified');
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: '/orchestrate-answer synthetic-run guidance' } });
  await fireEvent.submit(screen.getByRole('form', { name: 'Send a prompt' }));
  expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(current, { type: 'submit_prompt', text: '/orchestrate-answer synthetic-run guidance' });
});

it.each(['conversation', 'activity'])('escapes notice markup and retains legitimate code blocks in %s', async mode => {
  localStorage.setItem('herdr-conversation-view', mode);
  const text = '<img src=x onerror="alert(1)">\n\n```js\nconst answer = "safe";\n```';
  vi.mocked(relayStore.getConversationHistory).mockResolvedValue(page([{ ...notice, text }]));
  render(ConversationHistory, { agent });
  await vi.advanceTimersByTimeAsync(0);
  const article = document.querySelector('[data-conversation-entry-id="pi:notice"]')!;
  expect(article.querySelector('img,script,[onerror]')).toBeNull();
  expect(article.querySelector('pre code')).toHaveTextContent('const answer = "safe";');
});

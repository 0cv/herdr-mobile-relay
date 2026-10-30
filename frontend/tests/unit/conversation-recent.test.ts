import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import { ConversationHistoryController, conversationIdentity, HISTORY_WIRE_PAGE_SIZE } from '$lib/conversation-history';
import { clearConversationPreviews, getConversationPreview, putConversationPreview } from '$lib/conversation-cache';
import { relayStore } from '$lib/store';
import type { Agent, ConversationEntry, ConversationPage } from '$lib/types';

const agent: Agent = {
  relay_id: 'recent', relay_label: 'Recent', raw_pane_id: 'pane', pane_id: 'recent::pane',
  server_session_id: 'primary', terminal_id: 'terminal', generation: 1,
  agent_session_id: 'session', agent: 'pi', project: 'Recent', cwd: '/synthetic', status: 'working',
};
const rows = (start: number, end: number): ConversationEntry[] => Array.from({ length: end - start }, (_, i) => ({
  id: `entry-${start + i}`, timestamp: '', role: (start + i) % 2 ? 'assistant' : 'user', text: `message ${start + i}`,
}));
const page = (entries: ConversationEntry[], cursor = ''): ConversationPage => ({
  available: true, reason: '', hasMore: Boolean(cursor), nextCursor: cursor, total: null,
  entries, sourceRevision: 'revision', state: 'ready', mode: 'recent',
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  clearConversationPreviews();
});

it('reuses only a bounded cloned suffix of the exact memory preview without fetching context', async () => {
  const identity = conversationIdentity(agent)!;
  putConversationPreview({ identity, relayId: agent.relay_id, historical: false, sourceRevision: 'cached', savedAt: Date.now(), entries: rows(0, 1000) });
  const preview = getConversationPreview(identity, Date.now(), HISTORY_WIRE_PAGE_SIZE)!;
  expect(preview.entries).toHaveLength(24);
  preview.entries[0].text = 'changed';
  expect(getConversationPreview(identity)!.entries[976].text).toBe('message 976');
  expect(getConversationPreview(conversationIdentity({ ...agent, generation: 2 })!)).toBeNull();
  const request = vi.fn(() => new Promise<ConversationPage>(() => {}));
  const controller = new ConversationHistoryController(agent, { request, getPreview: getConversationPreview });
  controller.start();
  expect(controller.state.entries).toHaveLength(24);
  expect(controller.state.preview).toBe(true);
  expect(controller.state.authoritative).toBe(false);
  expect(request).toHaveBeenCalledTimes(1);
  controller.cancel();
});

it('does not fetch underflow or mode-switch context even when IntersectionObserver fires repeatedly', async () => {
  const observers: (() => void)[] = [];
  vi.stubGlobal('IntersectionObserver', class {
    constructor(callback: (entries: { isIntersecting: boolean }[]) => void) { observers.push(() => callback([{ isIntersecting: true }])); }
    observe() {}
    disconnect() {}
  });
  const request = vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue(page([{ id: 'fragment', role: 'assistant', timestamp: '', text: 'latest fragment' }], 'older'));
  const view = render(ConversationHistory, { agent });
  await screen.findByText('latest fragment');
  for (let i = 0; i < 5; i++) observers.forEach(callback => callback());
  await fireEvent.click(screen.getByRole('button', { name: 'Conversation' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Full history' }));
  await new Promise(resolve => setTimeout(resolve, 20));
  expect(request).toHaveBeenCalledTimes(1);
  expect(request.mock.calls[0][1]?.limit).toBe(24);
  expect(screen.getByText('latest fragment')).toBeVisible();
  expect(screen.getByRole('button', { name: 'Load older turns' })).toBeEnabled();
  view.unmount();
});

it('loads a single older page on upward demand and restores the same visible entry position', async () => {
  vi.stubGlobal('requestAnimationFrame', () => 1);
  vi.stubGlobal('cancelAnimationFrame', () => {});
  const request = vi.spyOn(relayStore, 'getConversationHistory')
    .mockResolvedValueOnce(page(rows(24, 48), 'older'))
    .mockResolvedValueOnce(page(rows(0, 24), 'earliest'));
  const view = render(ConversationHistory, { agent });
  await screen.findByText('message 47');
  const list = screen.getByRole('region', { name: 'Conversation with Recent' });
  let top = 1200;
  Object.defineProperties(list, {
    scrollHeight: { configurable: true, value: 2400 },
    clientHeight: { configurable: true, value: 500 },
    scrollTop: { configurable: true, get: () => top, set: (v: number) => { top = Math.min(1900, v); } },
  });
  vi.spyOn(list, 'getBoundingClientRect').mockReturnValue({ top: 0 } as DOMRect);
  const originalRect = HTMLElement.prototype.getBoundingClientRect;
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
    if (!this.matches('.conversation-entry')) return originalRect.call(this);
    const siblings = [...this.parentElement!.querySelectorAll('.conversation-entry')];
    const y = siblings.indexOf(this) * 100 - top;
    return { top: y, bottom: y + 100 } as DOMRect;
  });
  await fireEvent.pointerDown(list);
  await fireEvent.scroll(list);
  top = 0;
  await fireEvent.scroll(list);
  await vi.waitFor(() => expect(request).toHaveBeenCalledTimes(2));
  await vi.waitFor(() => expect(top).toBe(1200));
  const anchor = view.container.querySelector<HTMLElement>('[data-conversation-entry-id="entry-24"]')!;
  expect(anchor.getBoundingClientRect().top).toBe(0);
  expect(view.container.querySelectorAll('.conversation-entry')).toHaveLength(24);
  expect(request.mock.calls[1][1]?.cursor).toBe('older');
  await new Promise(resolve => setTimeout(resolve, 20));
  expect(request).toHaveBeenCalledTimes(2);
  view.unmount();
});

it('preserves live edits while paging and resets the target identity', async () => {
  let complete: (value: ConversationPage) => void = () => {};
  const request = vi.fn().mockResolvedValueOnce(page(rows(24, 48), 'older'))
    .mockImplementationOnce(() => new Promise<ConversationPage>(resolve => { complete = resolve; }))
    .mockResolvedValueOnce(page(rows(100, 124)));
  const controller = new ConversationHistoryController(agent, { request });
  controller.start();
  await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
  controller.demandOlder();
  expect(controller.applyLiveEntries('revision', [{ ...rows(47, 48)[0], text: 'live edit' }], 2)).toBe(true);
  complete(page([...rows(0, 24), rows(24, 25)[0]]));
  await vi.waitFor(() => expect(controller.state.requestPhase).toBe('idle'));
  expect(controller.state.entries).toHaveLength(48);
  expect(new Set(controller.state.entries.map(entry => entry.id)).size).toBe(48);
  expect(controller.state.entries.at(-1)?.text).toBe('live edit');
  controller.setAgent({ ...agent, generation: 2 });
  await vi.waitFor(() => expect(controller.state.entries[0]?.id).toBe('entry-100'));
  controller.cancel();
});

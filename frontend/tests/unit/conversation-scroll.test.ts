import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import { relayStore } from '$lib/store';
import type { Agent } from '$lib/types';

// WebKit may deliver the layout-induced scroll before ResizeObserver. A pin
// restored by the reader must survive that order even after the pin timer ends.
it('keeps a restored bottom pin through a delayed layout scroll, but respects upward scrolling', async () => {
  const resizeCallbacks: (() => void)[] = [];
  vi.stubGlobal('ResizeObserver', class {
    constructor(callback: () => void) { resizeCallbacks.push(callback); }
    observe() {}
    disconnect() {}
  });
  // Drive the resize ordering explicitly instead of racing animation frames.
  vi.stubGlobal('requestAnimationFrame', () => 1);
  vi.stubGlobal('cancelAnimationFrame', () => {});
  const current: Agent = {
    relay_id: 'scroll-test', relay_label: 'Scroll test', raw_pane_id: 'pane',
    pane_id: 'scroll-test::pane', server_session_id: 'primary', terminal_id: 'terminal',
    generation: 1, agent_session_id: 'session', agent: 'claude', project: 'Pin test', status: 'working',
  };
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({
    available: true, reason: '', hasMore: false, total: 2,
    state: 'ready', mode: 'recent', sourceRevision: 'revision',
    entries: [
      { id: 'question', timestamp: '', role: 'user', text: 'pin question' },
      { id: 'answer', timestamp: '', role: 'assistant', text: 'pin answer' },
    ],
  });
  const view = render(ConversationHistory, { agent: current });
  try {
    await screen.findByText('pin answer');
    const list = screen.getByRole('region', { name: 'Conversation with Pin test' });
    let height = 2000;
    let top = 0;
    Object.defineProperties(list, {
      scrollHeight: { configurable: true, get: () => height },
      clientHeight: { configurable: true, value: 500 },
      scrollTop: {
        configurable: true, get: () => top,
        set: (value: number) => { top = Math.max(0, Math.min(value, height - 500)); },
      },
    });
    const resize = () => resizeCallbacks.forEach((callback) => callback());
    expect(resizeCallbacks.length).toBeGreaterThan(0);
    resize();
    expect(top).toBe(1500);
    list.scrollTop = 1000;
    await fireEvent.scroll(list);
    list.scrollTop = 1500;
    await fireEvent.scroll(list);
    vi.spyOn(Date, 'now').mockReturnValue(Date.now() + 1000);
    height += 76;
    await fireEvent.scroll(list); // Layout scroll comes BEFORE resize delivery.
    await fireEvent.scroll(list); // Coalesced events must not release it either.
    resize();
    expect(top).toBe(1576);

    list.scrollTop = 1400;
    await fireEvent.scroll(list);
    height += 76;
    resize();
    expect(top).toBe(1400); // Real reader movement still releases the pin.
  } finally {
    view.unmount();
  }
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

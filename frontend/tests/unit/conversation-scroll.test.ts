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
    for (const position of [1497, 1497.5, 1498, 1498.5, 1499, 1499.5, 1500]) {
      list.scrollTop = position;
      await fireEvent.scroll(list);
    }
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

it('keeps taps pinned, resumes at the actual end, and releases immediately on upward movement', async () => {
  const resizes: (() => void)[] = [];
  vi.stubGlobal('ResizeObserver', class {
    constructor(callback: () => void) { resizes.push(callback); }
    observe() {}
    disconnect() {}
  });
  vi.stubGlobal('requestAnimationFrame', () => 1);
  vi.stubGlobal('cancelAnimationFrame', () => {});
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({
    available: true, reason: '', hasMore: false, total: 2,
    state: 'ready', mode: 'recent', sourceRevision: 'revision',
    entries: [
      { id: 'question', timestamp: '', role: 'user', text: 'touch question' },
      { id: 'answer', timestamp: '', role: 'assistant', text: 'touch answer' },
    ],
  });
  const current: Agent = {
    relay_id: 'touch-test', relay_label: 'Touch test', raw_pane_id: 'pane',
    pane_id: 'touch-test::pane', server_session_id: 'primary', terminal_id: 'terminal',
    generation: 1, agent_session_id: 'session', agent: 'pi', project: 'Touch test', status: 'working',
  };
  const view = render(ConversationHistory, { agent: current });
  try {
    await screen.findByText('touch answer');
    const list = screen.getByRole('region', { name: 'Conversation with Touch test' });
    let height = 2000;
    let viewport = 500;
    let top = 0;
    Object.defineProperties(list, {
      scrollHeight: { configurable: true, get: () => height },
      clientHeight: { configurable: true, get: () => viewport },
      scrollTop: {
        configurable: true, get: () => Math.min(top, height - viewport),
        set: (value: number) => { top = Math.max(0, Math.min(value, height - viewport)); },
      },
    });
    const resize = () => resizes.forEach(callback => callback());
    resize();
    await fireEvent.pointerDown(list, { pointerType: 'touch' });
    await fireEvent.focusIn(list);
    height += 100;
    resize();
    expect(list.scrollTop).toBe(1600);
    list.scrollTop = 1000;
    await fireEvent.scroll(list);
    list.scrollTop = 1582;
    await fireEvent.scroll(list);
    expect(list.scrollTop).toBe(1582);
    list.scrollTop = 1300;
    await fireEvent.scroll(list);
    height += 100;
    resize();
    expect(list.scrollTop).toBe(1300);
    list.scrollTop = height;
    await fireEvent.scroll(list);
    viewport = 700;
    await fireEvent.scroll(list);
    height += 100;
    resize();
    expect(list.scrollTop).toBe(1600);
  } finally {
    view.unmount();
  }
});

it.each([400, 600])('releases following when scrolling up while the viewport changes to %ipx', async (nextViewport) => {
  const resizes: (() => void)[] = [];
  vi.stubGlobal('ResizeObserver', class {
    constructor(callback: () => void) { resizes.push(callback); }
    observe() {}
    disconnect() {}
  });
  vi.stubGlobal('requestAnimationFrame', () => 1);
  vi.stubGlobal('cancelAnimationFrame', () => {});
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({
    available: true, reason: '', hasMore: false, total: 1,
    state: 'ready', mode: 'recent', sourceRevision: 'revision',
    entries: [{ id: 'answer', timestamp: '', role: 'assistant', text: 'resize answer' }],
  });
  const current: Agent = {
    relay_id: 'resize-test', relay_label: 'Resize test', raw_pane_id: 'pane',
    pane_id: 'resize-test::pane', server_session_id: 'primary', terminal_id: 'terminal',
    generation: 1, agent_session_id: 'session', agent: 'pi', project: 'Resize test', status: 'working',
  };
  const view = render(ConversationHistory, { agent: current });
  try {
    await screen.findByText('resize answer');
    const list = screen.getByRole('region', { name: 'Conversation with Resize test' });
    let height = 2000;
    let viewport = 500;
    let top = 0;
    Object.defineProperties(list, {
      scrollHeight: { configurable: true, get: () => height },
      clientHeight: { configurable: true, get: () => viewport },
      scrollTop: {
        configurable: true, get: () => Math.min(top, height - viewport),
        set: (value: number) => { top = Math.max(0, Math.min(value, height - viewport)); },
      },
    });
    const resize = () => resizes.forEach(callback => callback());
    resize();
    await fireEvent.pointerDown(list, { pointerType: 'touch' });
    viewport = nextViewport;
    list.scrollTop -= 120;
    const readingTop = list.scrollTop;
    await fireEvent.scroll(list);
    resize();
    expect(list.scrollTop).toBe(readingTop);
    height += 100;
    resize();
    expect(list.scrollTop).toBe(readingTop);
    list.scrollTop = height;
    await fireEvent.scroll(list);
    height += 100;
    resize();
    expect(list.scrollTop).toBe(height - viewport);
  } finally {
    view.unmount();
  }
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

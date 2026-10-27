import { fireEvent, render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, expect, it, vi } from 'vitest';
import TerminalView from '$components/TerminalView.svelte';
import type { Agent, TerminalFrame } from '$lib/types';

it.each([false, true])('restores terminal geometry after hidden scroll and resize callbacks (pinned=%s)', async (pinned) => {
  const observers: { callback: ResizeObserverCallback; targets: Set<Element> }[] = [];
  vi.stubGlobal('ResizeObserver', class {
    targets = new Set<Element>();
    constructor(readonly callback: ResizeObserverCallback) { observers.push(this); }
    observe(target: Element) { this.targets.add(target); }
    disconnect() { this.targets.clear(); }
  });
  vi.stubGlobal('requestAnimationFrame', () => 1);
  vi.stubGlobal('cancelAnimationFrame', () => {});
  const agent: Agent = {
    relay_id: 'terminal-scroll', relay_label: 'Terminal scroll', raw_pane_id: 'pane',
    pane_id: 'terminal-scroll::pane', server_session_id: 'primary', terminal_id: 'terminal',
    generation: 1, agent: 'codex', project: 'Scroll test', status: 'working',
  };
  const frame: TerminalFrame = {
    paneId: agent.pane_id, format: 'plain',
    content: Array.from({ length: 140 }, (_, index) => `line ${index}`).join('\n'),
  };
  const view = render(TerminalView, { agent, allAgents: [agent], responding: new Set<string>(), frame });
  await tick();
  await tick();
  const terminal = screen.getByRole('log');
  let clientHeight = 500;
  let top = 1500;
  Object.defineProperties(terminal, {
    scrollHeight: { configurable: true, value: 2000 },
    clientHeight: { configurable: true, get: () => clientHeight },
    clientWidth: { configurable: true, value: 300 },
    scrollTop: {
      configurable: true, get: () => top,
      set: (value: number) => { top = Math.max(0, Math.min(value, 2000 - clientHeight)); },
    },
  });
  top = pinned ? 1500 : 1420;
  await fireEvent.scroll(terminal);
  const before = top;
  const question: Agent = {
    ...agent, status: 'blocked', attention_kind: 'question', attention_capable: true,
    interaction: {
      id: 'scroll-question', kind: 'single_select', question: 'Choose coverage',
      options: [{ index: 0, label: 'Both' }],
    },
  };
  await view.rerender({ agent: question, allAgents: [question] });
  const pendingRows = [...terminal.querySelectorAll('[data-terminal-row]')];
  clientHeight = 1000;
  terminal.scrollTop = 1000;
  await fireEvent.scroll(terminal);
  for (const observer of observers) {
    const targets = [...observer.targets];
    const entries = targets.includes(terminal) ? [] : pendingRows.map((target) => ({
      target, contentRect: new DOMRectReadOnly(0, 0, 1, 60),
      borderBoxSize: [{ blockSize: 60, inlineSize: 1 }],
      contentBoxSize: [{ blockSize: 60, inlineSize: 1 }],
      devicePixelContentBoxSize: [{ blockSize: 60, inlineSize: 1 }],
    }));
    observer.callback(entries, {} as ResizeObserver);
  }
  const nextFrame = { ...frame, content: `${frame.content}\nnew output while hidden` };
  await view.rerender({ frame: nextFrame });
  const transcript = screen.getByRole('textbox', { name: 'Full terminal transcript', hidden: true }) as HTMLTextAreaElement;
  expect(transcript.value).not.toContain('new output while hidden');
  clientHeight = 500;
  await view.rerender({ agent, allAgents: [agent] });
  await tick();
  await tick();
  expect(top).toBe(before);
  expect(transcript.value).toContain('new output while hidden');
  view.unmount();
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

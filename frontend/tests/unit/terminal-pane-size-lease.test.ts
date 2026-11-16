import { render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import TerminalView from '$components/TerminalView.svelte';
import { PANE_LEASE_HIDDEN_GRACE_MS } from '$lib/config';
import { terminalHeightLease, terminalResizeSession } from '$lib/preferences';
import { securityState } from '$lib/security';
import { relayStore } from '$lib/store';
import type { Agent, TerminalFrame } from '$lib/types';
import { deferred, makeAgent } from '../support/relay';

type Applied = { columns: number; rows: number };

const CELL_WIDTH = 8;
const LINE_HEIGHT = 10;
const RENEWAL_MS = 10_000;
const RESIZE_WAIT_MS = 4_000;
const READER_CAPABILITIES = ['pane_size_opt_in'];
const ROW_CAPABILITIES = ['pane_size_opt_in', 'pane_size_opt_in_rows'];

const agent = makeAgent({ status: 'working' });
const questionAgent: Agent = {
  ...agent, status: 'blocked', attention_kind: 'question', attention_capable: true,
  interaction: {
    id: 'lease-question', kind: 'single_select', question: 'Choose coverage',
    options: [{ index: 0, label: 'Both' }],
  },
};

let measured = { width: 400, height: 300, cellWidth: CELL_WIDTH };
let visibility: DocumentVisibilityState = 'visible';
let typingField: HTMLInputElement | null = null;

const isCellProbe = (element: Element) => element.getAttribute('aria-hidden') === 'true';
const echoLease = async (_target: Agent, columns: number, rows = 0): Promise<Applied> => ({ columns, rows });

function stubMeasurements() {
  const rect = Element.prototype.getBoundingClientRect;
  vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
    if (isCellProbe(this)) return { width: measured.cellWidth * 10 } as DOMRect;
    return rect.call(this);
  });
  const sizeOf = (property: 'width' | 'height') => function (this: Element) {
    return this.classList.contains('term-content') ? measured[property] : 0;
  };
  Object.defineProperty(Element.prototype, 'clientWidth', { configurable: true, get: sizeOf('width') });
  Object.defineProperty(Element.prototype, 'clientHeight', { configurable: true, get: sizeOf('height') });
  const computed = window.getComputedStyle;
  vi.spyOn(window, 'getComputedStyle').mockImplementation(((element: Element, pseudo?: string | null) => {
    if (!element.classList.contains('term-content')) return computed(element, pseudo);
    return {
      paddingLeft: '0px', paddingRight: '0px', paddingTop: '0px', paddingBottom: '0px',
      lineHeight: `${LINE_HEIGHT}px`,
    } as CSSStyleDeclaration;
  }) as typeof window.getComputedStyle);
}

function setConnection(status: 'connected' | 'disconnected', capabilities = READER_CAPABILITIES) {
  relayStore.connections.set(new Map([[agent.relay_id, { status, capabilities }]]) as never);
}

function setVisibility(next: DocumentVisibilityState) {
  visibility = next;
  document.dispatchEvent(new Event('visibilitychange'));
}

async function flush() {
  for (let round = 0; round < 3; round += 1) {
    await vi.advanceTimersByTimeAsync(0);
    await tick();
  }
}

async function advance(ms: number) {
  await vi.advanceTimersByTimeAsync(ms);
  await tick();
}

async function resizeTo(next: Partial<typeof measured>) {
  measured = { ...measured, ...next };
  window.dispatchEvent(new Event('resize'));
  await flush();
}

function frameOf(content: string, extra: Partial<TerminalFrame> = {}): TerminalFrame {
  return { paneId: agent.pane_id, format: 'plain', content, ...extra };
}

function mountView(options: { capabilities?: string[]; heightLease?: boolean; frame?: TerminalFrame; agent?: Agent } = {}) {
  setConnection('connected', options.capabilities);
  terminalResizeSession.set(true);
  terminalHeightLease.set(options.heightLease ?? false);
  const target = options.agent ?? agent;
  return render(TerminalView, {
    agent: target, allAgents: [target], responding: new Set<string>(), readOnly: false, frame: options.frame,
  });
}

function forcedReads() {
  return vi.mocked(relayStore.readPane).mock.calls.filter(([, force]) => force === true);
}

function terminal() {
  return screen.getByRole('log');
}

beforeEach(() => {
  vi.useFakeTimers();
  measured = { width: 400, height: 300, cellWidth: CELL_WIDTH };
  visibility = 'visible';
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility });
  stubMeasurements();
  vi.spyOn(relayStore, 'leasePaneSize').mockImplementation(echoLease);
  vi.spyOn(relayStore, 'releasePaneSize').mockResolvedValue(undefined);
  vi.spyOn(relayStore, 'showToast');
  for (const method of ['readPane', 'watchPane', 'unwatchPane'] as const) {
    vi.spyOn(relayStore, method).mockImplementation(() => {});
  }
});

afterEach(() => {
  typingField?.remove();
  typingField = null;
  vi.restoreAllMocks();
  vi.useRealTimers();
  delete (document as { visibilityState?: unknown }).visibilityState;
  delete (Element.prototype as { clientWidth?: unknown }).clientWidth;
  delete (Element.prototype as { clientHeight?: unknown }).clientHeight;
  relayStore.connections.set(new Map());
  terminalResizeSession.set(false);
  terminalHeightLease.set(false);
  securityState.update((state) => ({ ...state, locked: false }));
});

describe('first lease', () => {
  it('leases the measured width on mount, with no rows while the height switch is off', async () => {
    const view = mountView();
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(1);
    expect(relayStore.leasePaneSize).toHaveBeenCalledWith(agent, 50, 0);
    expect(terminal()).toHaveClass('resize-layout');
    view.unmount();
  });
});

describe('lease result', () => {
  it('reads the pane again when the applied size changes and not when it stays the same', async () => {
    const view = mountView();
    await flush();
    vi.mocked(relayStore.readPane).mockClear();

    await advance(RENEWAL_MS);
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(2);
    expect(forcedReads()).toHaveLength(0);

    await resizeTo({ width: 480 });
    expect(relayStore.leasePaneSize).toHaveBeenLastCalledWith(agent, 60, 0);
    expect(forcedReads()).toEqual([[agent, true]]);
    view.unmount();
  });

  it('shows the failure, drops the resize layout and clears it on the next good lease', async () => {
    const view = mountView();
    await flush();
    expect(terminal()).toHaveClass('resize-layout');

    vi.mocked(relayStore.leasePaneSize).mockRejectedValueOnce(new Error('relay said no'));
    await advance(RENEWAL_MS);
    expect(screen.getByRole('alert')).toHaveTextContent('Resize Session failed: relay said no');
    expect(terminal()).not.toHaveClass('resize-layout');

    await advance(RENEWAL_MS);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(terminal()).toHaveClass('resize-layout');
    view.unmount();
  });

  it('says so when the terminal cell cannot be measured and leases nothing', async () => {
    measured.cellWidth = 0;
    const view = mountView();
    await flush();
    expect(screen.getByRole('alert')).toHaveTextContent('Resize Session could not measure the terminal cell width.');
    expect(relayStore.leasePaneSize).not.toHaveBeenCalled();
    view.unmount();
  });

  it.each([
    { name: 'height switch off', heightLease: false, capabilities: ROW_CAPABILITIES, rows: 0 },
    { name: 'relay without row support', heightLease: true, capabilities: READER_CAPABILITIES, rows: 0 },
    { name: 'height switch on and row support', heightLease: true, capabilities: ROW_CAPABILITIES, rows: 30 },
  ])('sends rows only with the height switch and relay support: $name', async ({ heightLease, capabilities, rows }) => {
    const view = mountView({ heightLease, capabilities });
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledWith(agent, 50, rows);
    view.unmount();
  });
});

describe('queueing and timing', () => {
  it('sends one follow-up lease with the latest size after a lease in flight', async () => {
    const first = deferred<Applied>();
    vi.mocked(relayStore.leasePaneSize).mockImplementationOnce(() => first.promise);
    const view = mountView();
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(1);

    await resizeTo({ width: 480 });
    await resizeTo({ width: 520 });
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(1);

    first.resolve({ columns: 50, rows: 0 });
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(2);
    expect(relayStore.leasePaneSize).toHaveBeenLastCalledWith(agent, 65, 0);
    view.unmount();
  });

  it('does not send an unchanged size again unless the renewal forces it', async () => {
    const view = mountView();
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(1);

    await resizeTo({});
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(1);

    await advance(RENEWAL_MS);
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(2);
    expect(relayStore.leasePaneSize).toHaveBeenLastCalledWith(agent, 50, 0);
    view.unmount();
  });

  it('renews every ten seconds', async () => {
    const view = mountView();
    await flush();
    await advance(RENEWAL_MS * 3);
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(4);
    view.unmount();
  });

  it('keeps renewing while hidden inside the grace and stops after it', async () => {
    const view = mountView();
    await flush();
    setVisibility('hidden');
    await flush();
    const before = vi.mocked(relayStore.leasePaneSize).mock.calls.length;

    await advance(PANE_LEASE_HIDDEN_GRACE_MS - RENEWAL_MS);
    const insideGrace = vi.mocked(relayStore.leasePaneSize).mock.calls.length;
    expect(insideGrace).toBeGreaterThan(before);

    await advance(PANE_LEASE_HIDDEN_GRACE_MS);
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(insideGrace);
    view.unmount();
  });

  it('stops renewing at once when the app is locked, without the grace, and leases again on unlock', async () => {
    const view = mountView();
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(1);

    securityState.update((state) => ({ ...state, locked: true }));
    await flush();
    await advance(RENEWAL_MS * 3);
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(1);

    securityState.update((state) => ({ ...state, locked: false }));
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(2);
    expect(relayStore.leasePaneSize).toHaveBeenLastCalledWith(agent, 50, 0);
    view.unmount();
  });

  it('leases again at once when the page becomes visible after the grace ran out', async () => {
    const view = mountView();
    await flush();
    setVisibility('hidden');
    await advance(PANE_LEASE_HIDDEN_GRACE_MS + RENEWAL_MS);
    const hiddenCalls = vi.mocked(relayStore.leasePaneSize).mock.calls.length;

    setVisibility('visible');
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(hiddenCalls + 1);
    expect(relayStore.leasePaneSize).toHaveBeenLastCalledWith(agent, 50, 0);
    view.unmount();
  });
});

describe('release paths', () => {
  it('releases when a question appears and leases again when it is answered', async () => {
    const view = mountView();
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(1);

    await view.rerender({ agent: questionAgent, allAgents: [questionAgent] });
    await flush();
    expect(relayStore.releasePaneSize).toHaveBeenCalledTimes(1);
    expect(relayStore.releasePaneSize).toHaveBeenCalledWith(agent);
    expect(terminal()).not.toHaveClass('resize-layout');

    await view.rerender({ agent, allAgents: [agent] });
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(2);
    expect(relayStore.leasePaneSize).toHaveBeenLastCalledWith(expect.objectContaining({ pane_id: agent.pane_id }), 50, 0);
    view.unmount();
  });

  it('releases on unmount and never shows an alert about it', async () => {
    vi.mocked(relayStore.releasePaneSize).mockRejectedValue(new Error('gone'));
    const view = mountView();
    await flush();
    view.unmount();
    await flush();
    expect(relayStore.releasePaneSize).toHaveBeenCalledTimes(1);
    expect(relayStore.releasePaneSize).toHaveBeenCalledWith(agent);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(relayStore.showToast).not.toHaveBeenCalled();
  });

  it('shows a failed release while the view is mounted and the relay connected', async () => {
    vi.mocked(relayStore.releasePaneSize).mockRejectedValue(new Error('release refused'));
    const view = mountView();
    await flush();
    await view.rerender({ agent: questionAgent, allAgents: [questionAgent] });
    await flush();
    expect(screen.getByRole('alert')).toHaveTextContent('Resize Session release failed: release refused');
    view.unmount();
  });

  it('stays quiet about a failed release once the relay is disconnected', async () => {
    const release = deferred<void>();
    vi.mocked(relayStore.releasePaneSize).mockReturnValue(release.promise);
    const view = mountView();
    await flush();
    await view.rerender({ agent: questionAgent, allAgents: [questionAgent] });
    await flush();
    expect(relayStore.releasePaneSize).toHaveBeenCalledTimes(1);

    setConnection('disconnected');
    await flush();
    release.reject(new Error('release refused'));
    await flush();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    view.unmount();
  });

  it('drops the local lease when the preference is turned off and leases again when it is turned on', async () => {
    const view = mountView();
    await flush();
    expect(terminal()).toHaveClass('resize-layout');

    terminalResizeSession.set(false);
    await flush();
    expect(terminal()).not.toHaveClass('resize-layout');
    expect(relayStore.releasePaneSize).not.toHaveBeenCalled();

    terminalResizeSession.set(true);
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(2);
    expect(relayStore.leasePaneSize).toHaveBeenLastCalledWith(agent, 50, 0);
    view.unmount();
  });

  it.each(['resolves', 'rejects'] as const)(
    'ignores a lease reply that %s after the preference was turned off',
    async (outcome) => {
      const reply = deferred<Applied>();
      vi.mocked(relayStore.leasePaneSize).mockImplementationOnce(() => reply.promise);
      const view = mountView();
      await flush();
      vi.mocked(relayStore.readPane).mockClear();

      terminalResizeSession.set(false);
      await flush();
      if (outcome === 'resolves') reply.resolve({ columns: 50, rows: 0 });
      else reply.reject(new Error('too late'));
      await flush();

      expect(terminal()).not.toHaveClass('resize-layout');
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(forcedReads()).toHaveLength(0);
      view.unmount();
    },
  );
});

describe('keyboard rule and resize wait', () => {
  function focusTypingField() {
    typingField = document.createElement('input');
    document.body.append(typingField);
    typingField.focus();
    expect(document.activeElement).toBe(typingField);
  }

  it('lets the leased rows shrink when no text input has focus', async () => {
    const view = mountView({ heightLease: true, capabilities: ROW_CAPABILITIES });
    await flush();
    await resizeTo({ height: 200 });
    expect(relayStore.leasePaneSize).toHaveBeenLastCalledWith(agent, 50, 20);
    view.unmount();
  });

  it('keeps the leased rows while a text input has focus, but lets them grow', async () => {
    const view = mountView({ heightLease: true, capabilities: ROW_CAPABILITIES });
    await flush();
    expect(relayStore.leasePaneSize).toHaveBeenLastCalledWith(agent, 50, 30);
    focusTypingField();

    await resizeTo({ height: 200 });
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(1);

    await resizeTo({ height: 400 });
    expect(relayStore.leasePaneSize).toHaveBeenCalledTimes(2);
    expect(relayStore.leasePaneSize).toHaveBeenLastCalledWith(agent, 50, 40);
    view.unmount();
  });

  it('keeps the previous frame, then ignores settling frames, until a settled frame arrives', async () => {
    const before = frameOf('screen before resize');
    const view = mountView({ frame: before });
    await flush();
    expect(terminal()).toHaveTextContent('screen before resize');

    await view.rerender({ frame: frameOf('half drawn screen', { resizeSettling: true }) });
    await flush();
    expect(terminal()).toHaveTextContent('screen before resize');
    expect(terminal()).not.toHaveTextContent('half drawn screen');

    await view.rerender({ frame: frameOf('screen after resize') });
    await flush();
    expect(terminal()).toHaveTextContent('screen after resize');
    view.unmount();
  });

  it('shows the settling frame once the resize wait has run out', async () => {
    const view = mountView({ frame: frameOf('screen before resize') });
    await flush();
    await view.rerender({ frame: frameOf('half drawn screen', { resizeSettling: true }) });
    await flush();

    await advance(RESIZE_WAIT_MS - 1);
    expect(terminal()).toHaveTextContent('screen before resize');
    await advance(1);
    await flush();
    expect(terminal()).toHaveTextContent('half drawn screen');
    view.unmount();
  });

  it('shows "Resizing terminal…" while there is nothing to keep and the lease is outstanding, then gives up', async () => {
    const reply = deferred<Applied>();
    vi.mocked(relayStore.leasePaneSize).mockImplementationOnce(() => reply.promise);
    const view = mountView();
    await flush();
    expect(terminal()).toHaveTextContent('Resizing terminal…');

    await advance(RESIZE_WAIT_MS);
    await flush();
    expect(terminal()).toHaveTextContent('Loading…');
    expect(terminal()).not.toHaveTextContent('Resizing terminal…');
    view.unmount();
  });
});

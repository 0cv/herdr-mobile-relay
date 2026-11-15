<script lang="ts">
  import { onDestroy, onMount, tick, untrack } from 'svelte';
  import PromptComposer from '$components/PromptComposer.svelte';
  import TerminalKeys, { type QueuedKeyCommand } from '$components/TerminalKeys.svelte';
  import AgentInteraction from '$components/AgentInteraction.svelte';
  import TerminalInputMode from '$components/TerminalInputMode.svelte';
  import TerminalFind from '$components/TerminalFind.svelte';
  import TerminalResponseActions from '$components/TerminalResponseActions.svelte';
  import { promptDispatchRestriction } from '$lib/terminal-input';
  import type { ComposerSubmitAdapter } from '$lib/composer';
  import Button from '$components/ui/Button.svelte';
  import KeyboardIcon from '$components/ui/KeyboardIcon.svelte';
  import {
    MAX_PANE_SIZE_COLUMNS,
    MAX_PANE_SIZE_ROWS,
    MIN_PANE_SIZE_COLUMNS,
    MIN_PANE_SIZE_ROWS,
  } from '$lib/config';
  import {
    agentNeedsInspection,
    agentNeedsResponse,
    attentionKind,
    questionInteraction,
    sortedAgents,
  } from '$lib/agents';
  import { interfaceSize, terminalHeightLease, terminalResizeSession, theme } from '$lib/preferences';
  import { replaceView } from '$lib/router';
  import { targetRefForAgent } from '$lib/resource-id';
  import { securityState } from '$lib/security';
  import { paneActive } from '$lib/pane-activity.svelte';
  import { PaneSizeLease } from '$lib/pane-size-lease.svelte';
  import { terminalInputGate } from '$lib/terminal-input-gate.svelte';
  import { relayStore } from '$lib/store';
  import {
    renderedRowShift,
    stripAnsi,
    TERMINAL_SEPARATOR_TOKEN,
    renderTerminalContent,
    terminalResizeLayoutEngaged,
    terminalScreenColumns,
  } from '$lib/terminal';
  import { detectTerminalMenu, terminalTextInputMode } from '$lib/terminal-menu';
  import type {
    Agent,
    TerminalFrame,
  } from '$lib/types';
  import type { RenderedTerminalRow } from '$lib/terminal';
  import { VirtualTerminalIndex } from '$lib/virtual-terminal';
  import { mountTerminalWakeLock } from '$lib/wake-lock';

  const connections = relayStore.connections;

  let {
    agent,
    allAgents,
    frame,
    responding,
    readOnly = false,
  }: {
    agent: Agent;
    allAgents: Agent[];
    frame?: TerminalFrame;
    responding: Set<string>;
    readOnly?: boolean;
  } = $props();

  interface VirtualTerminalAnchor {
    index: number;
    offset: number;
    text: string;
  }


  let terminalElement = $state<HTMLDivElement>(null!);
  let cellMeasureElement = $state<HTMLSpanElement>(null!);
  let historyTruncated = $state(false);
  let displayed = $state('');
  let renderedHtml = $state('');
  let renderedRows = $state<RenderedTerminalRow[]>([]);
  let virtualHtml = $state('');
  let virtualTopHeight = $state(0);
  let virtualBottomHeight = $state(0);
  let virtualContentColumns = $state(0);
  let virtualStart = 0;
  let virtualEnd = 0;
  let virtualLayoutSignature = '';
  let virtualStickToBottom = true;
  let virtualScrollResetPending = false;
  let terminalLayoutSuspended = $state(false);
  let questionScrollState: { top: number; left: number; stick: boolean } | null = null;
  let pendingResizeAnchor: VirtualTerminalAnchor | null = null;
  let pendingResizeStick: boolean | null = null;
  let pendingLayoutStick: boolean | null = null;
  let virtualScrollTop = 0;
  let virtualClientHeight = 0;
  let virtualWindowFrame = 0;
  let virtualRowObserver: ResizeObserver | undefined;
  let virtualHeightCache = new Map<string, number>();
  const virtualIndex = new VirtualTerminalIndex();
  const wideGridOffsets = new Map<number, number>();
  let lastFormat = '';
  let lastContent = '';
  let lastPreserveLineEnds = false;
  let lastRenderColumnCap = 0;
  let findOpen = $state(false);
  let find = $state<TerminalFind>();
  let sendingPrompt = $state(false);
  let keySending = $state(false);
  const keyQueue: QueuedKeyCommand[] = [];
  onDestroy(() => {
    for (const command of keyQueue.splice(0)) command.resolve(false);
  });
  let terminalKeys = $state<TerminalKeys>();
  let terminalInputMode = $state<TerminalInputMode>();
  let manualInputLoading = $state(false);
  let manualInputTriggerHost = $state<HTMLElement>();
  let requestedPaneId = '';
  let dismissedMenuSignature = $state('');
  const CELL_MEASURE_TEXT = '0000000000';
  const PANE_REALTIME_RESYNC_MS = 15_000;
  let measuredCellWidth = $state(0);
  const lease = new PaneSizeLease({
    agent: () => agent,
    supported: paneSizeLeaseSupported,
    locked: () => $securityState.locked,
    visible: () => document.visibilityState === 'visible',
    rowsEnabled: paneSizeRowLeaseSupported,
    measure: (withRows) => {
      const columns = measuredPaneColumns();
      return {
        columns,
        rows: columns !== null && withRows ? measuredPaneRows() : 0,
        measurable: Boolean(terminalElement && cellMeasureElement),
      };
    },
    typing: () => document.activeElement instanceof HTMLTextAreaElement
      || document.activeElement instanceof HTMLInputElement,
    frame: () => frame,
    lease: (target, columns, rows) => relayStore.leasePaneSize(target, columns, rows),
    release: (target) => relayStore.releasePaneSize(target),
    applied: (target) => relayStore.readPane(target, true),
    canReportRelease: (target) => $connections.get(target.relay_id)?.status === 'connected',
  });
  // A lock flip changes what "visible" means for this pane, and no visibility
  // event comes with it, so the pane handler is called directly.
  let paneVisibilityChanged: (() => void) | null = null;
  let paneLocked = false;
  const responsePending = $derived(agentNeedsResponse(agent));
  const approvalMode = $derived(responsePending && attentionKind(agent) === 'approval');
  const inspectionMode = $derived(agentNeedsInspection(agent));
  const interaction = $derived(questionInteraction(agent));
  const questionMode = $derived(Boolean(!readOnly && responsePending && attentionKind(agent) === 'question' && interaction));
  // Resize Session needs three things: this device opted in, it controls the
  // relay, and the relay advertises the opt-in capability. The legacy
  // pane_size_lease name is ignored: a relay offering only that resizes on any
  // lease, with no opt-in check.
  const resizeSessionActive = $derived(
    Boolean($terminalResizeSession
      && !readOnly && $connections.get(agent.relay_id)?.capabilities.includes('pane_size_opt_in')),
  );
  // The capability only says the relay can lease; it does not mean this pane
  // has a width yet. The wrapping layout is engaged solely when it does.
  const resizeLayoutActive = $derived(terminalResizeLayoutEngaged(
    resizeSessionActive,
    measuredCellWidth,
    lease.layoutColumns,
  ));
  // Three regimes, not two. A relay that cannot lease at all serves a pane at
  // its own width, and the fixed layout keeps that pane's columns aligned and
  // scrolling sideways. A relay that can lease but has not granted a width yet
  // is showing a pane whose width is nobody's: alignment is already lost, so
  // those rows wrap at the container instead of stranding the reader on line
  // tails after every refresh. Readers use the same wrapping regime because
  // their role can never obtain a size lease.
  const resizeLayoutPending = $derived(readOnly || (resizeSessionActive && !resizeLayoutActive));
  const nextBlocked = $derived(sortedAgents(allAgents.filter((item) => agentNeedsResponse(item) && item.pane_id !== agent.pane_id))[0]);
  const terminalPlainText = $derived(
    stripAnsi(displayed)
      .replaceAll(TERMINAL_SEPARATOR_TOKEN, '────────'),
  );
  const terminalTextMode = $derived.by(() => {
    const mode = terminalTextInputMode(terminalPlainText);
    if (mode === 'filter') return mode;
    return null;
  });
  const sendingFilter = $derived(terminalTextMode === 'filter' && sendingPrompt);
  const inputLocked = $derived(readOnly || responsePending || inspectionMode || terminalTextMode === 'filter');
  let manualInput = $state(false);
  const inputGate = terminalInputGate(() => agent);
  const inputBlocked = $derived(inputGate.blocked);
  // Literal terminal input is the escape hatch for when the composer cannot send: the agent awaits a structured
  // response or needs inspection, or an earlier terminal send is unconfirmed and must be inspected first.
  const manualInputOffered = $derived(!readOnly && (Boolean(promptDispatchRestriction(agent)) || inputBlocked));
  const manualInputBusy = $derived(sendingPrompt || keySending || responding.has(agent.pane_id));
  const dispatchBlocked = $derived(responsePending || !terminalTextMode ? promptDispatchRestriction(agent) : '');
  const submitAdapter = $derived<ComposerSubmitAdapter | undefined>(terminalTextMode ? {
    filter: true,
    validate: (text) => keySending ? 'Wait for the pending terminal key.'
      : !/^[a-zA-Z0-9 ._/:+()[\]-]{1,32}$/.test(text) || text.endsWith(' ')
        ? 'Use 1–32 letters, digits, spaces or .-_/+:()[]; no trailing space.' : '',
    send: (target, text) => relayStore.sendToAgent(target, { type: 'send_filter_text', text, activity_label: 'Sent filter text' }, 15_000),
    sentMessage: 'Filter text sent. Select separately using terminal controls.',
    failureMessage: 'Filter text could not be sent.',
  } : undefined);
  const terminalMenu = $derived(detectTerminalMenu(terminalPlainText));
  const visibleTerminalMenu = $derived(
    !approvalMode
    && !questionMode
    && terminalMenu
    && terminalMenu.signature !== dismissedMenuSignature
      ? terminalMenu
      : null,
  );
  const terminalContentStyle = $derived.by(() => {
    // Every width is emitted in px of the measured probe cell, never in ch:
    // Safari's Core Text port resolves 1ch against different font metrics
    // than the rendered glyph advance, so an Nch cap wraps short of N cells.
    // Before the probe is measured no cap is emitted at all. The wrapping
    // layout is gated on the same predicate, so the missing cap can no longer
    // fall back to the container width and shred rows mid-word: the fixed
    // layout applies instead until a real cap exists.
    if (measuredCellWidth <= 0) return undefined;
    const capColumns = lease.layoutColumns;
    const styles = [`--terminal-cell-width: ${measuredCellWidth.toFixed(4)}px`];
    if (resizeLayoutActive) {
      styles.push(`--terminal-width: ${(capColumns * measuredCellWidth).toFixed(4)}px`);
    }
    if (virtualContentColumns > 0) {
      styles.push(`--terminal-content-width: ${(virtualContentColumns * measuredCellWidth).toFixed(4)}px`);
    }
    return styles.join(';');
  });

  // The ANSI palette follows the theme's terminal scheme, so a theme change
  // must repaint the cached frame rather than wait for the next output.
  $effect(() => {
    void $theme;
    untrack(() => {
      if (!lastContent) return;
      const next = frame;
      if (!next || next.paneId !== agent.pane_id) return;
      lastContent = '';
      void applyFrame(next, lastPreserveLineEnds);
    });
  });

  $effect(() => {
    if (questionMode || terminalLayoutSuspended) return;
    const next = frame;
    const matchingFrame = next?.paneId === agent.pane_id ? next : undefined;
    // Keyed to the session, not to resizeLayoutActive, on purpose: a relay that
    // can lease will repaint this pane at the phone's width, so trailing padding
    // measured at the desktop width is stale whether or not the width has landed
    // yet. A reader cannot lease at all, so its rows wrap at the phone instead
    // of trailing off the side of a desktop-width grid.
    const preserveLineEnds = !resizeSessionActive && !readOnly;
    // A frame read while the agent repaints at a new width is transient. Keep
    // the phone's last stable frame painted until the new stable frame lands,
    // but never past the deadline: a relay that keeps flagging frames would
    // otherwise freeze this pane on a screen that is minutes old.
    const waitingForResizedFrame = resizeSessionActive
      && !lease.error
      && !lease.waitExpired
      && (lease.columns === 0
        || (Boolean(lease.frameBaseline)
          && (next === lease.frameBaseline || next?.resizeSettling === true)));
    const cachedFrame = waitingForResizedFrame
      && matchingFrame
      && matchingFrame.resizeSettling !== true
      && (!lease.frameBaseline || matchingFrame === lease.frameBaseline)
        ? matchingFrame
        : undefined;
    if (waitingForResizedFrame) {
      untrack(() => {
        if (pendingResizeStick !== null) return;
        pendingResizeStick = virtualStickToBottom;
        pendingResizeAnchor = virtualStickToBottom
          ? null
          : currentVirtualAnchor(terminalElement?.scrollTop || 0);
      });
      if (cachedFrame) {
        historyTruncated = Boolean(cachedFrame.truncated);
        untrack(() => { void applyFrame(cachedFrame, preserveLineEnds) });
        return;
      }
      if (renderedRows.length) return;
    }
    if (!matchingFrame || waitingForResizedFrame) {
      untrack(() => {
        if (!waitingForResizedFrame) {
          pendingResizeStick = null;
          pendingResizeAnchor = null;
        }
        const message = waitingForResizedFrame ? 'Resizing terminal…' : 'Loading…';
        const rendered = renderTerminalContent(message, 'plain');
        displayed = rendered.display;
        renderedHtml = rendered.html;
        renderedRows = rendered.rows;
        resetVirtualRows(Number.POSITIVE_INFINITY);
        lastFormat = '';
        lastContent = '';
      });
      return;
    }
    if (resizeSessionActive && lease.columns > 0) lease.renderedColumns = lease.columns;
    historyTruncated = Boolean(matchingFrame.truncated);
    untrack(() => { void applyFrame(matchingFrame, preserveLineEnds) });
  });

  $effect(() => {
    const paneId = agent.pane_id;
    const connected = $connections.get(agent.relay_id)?.status === 'connected';
    if (!connected) {
      requestedPaneId = '';
      return;
    }
    if (resizeSessionActive && !lease.error) return;
    if (paneId === requestedPaneId) return;
    requestedPaneId = paneId;
    relayStore.readPane(agent);
  });

  $effect(() => {
    const locked = $securityState.locked;
    if (locked === paneLocked) return;
    paneLocked = locked;
    paneVisibilityChanged?.();
  });

  $effect(() => {
    const interfaceSizeValue = $interfaceSize;
    void interfaceSizeValue;
    lease.sync({
      enabled: $terminalResizeSession,
      active: resizeSessionActive,
      questionMode,
      connected: $connections.get(agent.relay_id)?.status === 'connected',
    });
  });

  $effect.pre(() => {
    const hidden = questionMode;
    const element = terminalElement;
    untrack(() => {
      if (!element) return;
      if (hidden) {
        if (questionScrollState) return;
        questionScrollState = {
          top: element.scrollTop,
          left: element.scrollLeft,
          stick: virtualStickToBottom,
        };
        terminalLayoutSuspended = true;
        virtualRowObserver?.disconnect();
        if (virtualWindowFrame) cancelAnimationFrame(virtualWindowFrame);
        virtualWindowFrame = 0;
        return;
      }
      const saved = questionScrollState;
      if (!saved) return;
      void tick().then(async () => {
        if (questionMode || questionScrollState !== saved || terminalElement !== element) return;
        virtualStickToBottom = saved.stick;
        renderVirtualWindow(saved.stick ? Number.POSITIVE_INFINITY : saved.top, true);
        await tick();
        if (questionMode || questionScrollState !== saved || terminalElement !== element) return;
        element.scrollTop = saved.stick ? element.scrollHeight : saved.top;
        element.scrollLeft = saved.left;
        rememberVirtualScrollGeometry(element);
        virtualScrollResetPending = false;
        questionScrollState = null;
        terminalLayoutSuspended = false;
        observeVirtualRows();
      });
    });
  });

  $effect.pre(() => {
    const interfaceSizeValue = $interfaceSize;
    void interfaceSizeValue;
    if (!terminalElement || terminalLayoutSuspended) return;
    pendingLayoutStick = virtualStickToBottom;
  });

  function rememberVirtualScrollGeometry(element: HTMLElement) {
    virtualScrollTop = element.scrollTop;
    virtualClientHeight = element.clientHeight;
  }

  function resetVirtualScroll(element: HTMLElement, stick: boolean) {
    if (terminalLayoutSuspended) return;
    virtualScrollResetPending = true;
    virtualLayoutSignature = '';
    const nextTop = resetVirtualRows(stick ? Number.POSITIVE_INFINITY : element.scrollTop);
    void tick().then(() => {
      if (terminalLayoutSuspended) return;
      element.scrollTop = stick ? element.scrollHeight : nextTop;
      rememberVirtualScrollGeometry(element);
      if (stick) virtualStickToBottom = true;
      virtualScrollResetPending = false;
    });
  }


  $effect(() => {
    const element = terminalElement;
    const interfaceSizeValue = $interfaceSize;
    void interfaceSizeValue;
    if (!element || typeof ResizeObserver === 'undefined') return;
    let previousWidth = element.clientWidth;
    let previousHeight = element.clientHeight;
    untrack(() => {
      if (!renderedRows.length) return;
      const stick = virtualStickToBottom;
      resetVirtualScroll(element, stick);
    });
    const observer = new ResizeObserver(() => {
      if (terminalLayoutSuspended) return;
      const nextWidth = element.clientWidth;
      const nextHeight = element.clientHeight;
      const widthChanged = Math.abs(nextWidth - previousWidth) >= 1;
      const heightChanged = Math.abs(nextHeight - previousHeight) >= 1;
      if (renderedRows.length && widthChanged) {
        resetVirtualScroll(element, virtualStickToBottom);
      } else if (heightChanged && virtualStickToBottom) {
        jumpToBottom();
      } else {
        scheduleVirtualWindow();
      }
      previousWidth = nextWidth;
      previousHeight = nextHeight;
      lease.request(false);
    });
    observer.observe(element);
    return () => observer.disconnect();
  });

  onMount(() => {
    lease.start();
    relayStore.readPane(agent, true);
    const stopWakeLock = mountTerminalWakeLock();
    const measurePane = () => lease.request(false);
    const realtimeDeltaEnabled = () => Boolean(
      $connections.get(agent.relay_id)?.capabilities.includes('pane_realtime_delta'),
    );
    let lastRefreshAt = Date.now();
    const visibilityChanged = () => {
      if (!paneActive.current) {
        lease.pageHidden();
        relayStore.unwatchPane(agent);
        return;
      }
      lease.pageShown();
      lastRefreshAt = Date.now();
      relayStore.readPane(agent);
      relayStore.watchPane(agent);
      // Re-lease at once: if the lease lapsed while asleep, the pane is back
      // at the desktop width until this lands.
      lease.request(true);
    };
    paneVisibilityChanged = visibilityChanged;
    window.addEventListener('resize', measurePane);
    window.visualViewport?.addEventListener('resize', measurePane);
    document.addEventListener('visibilitychange', visibilityChanged);
    const refresh = setInterval(() => {
      if (!paneActive.current) return;
      const refreshInterval = realtimeDeltaEnabled() ? PANE_REALTIME_RESYNC_MS : 3_000;
      if (Date.now() - lastRefreshAt < refreshInterval) return;
      lastRefreshAt = Date.now();
      relayStore.readPane(agent);
    }, 3_000);
    if (paneActive.current) relayStore.watchPane(agent);
    void tick().then(measurePane);
    return () => {
      window.removeEventListener('resize', measurePane);
      window.visualViewport?.removeEventListener('resize', measurePane);
      document.removeEventListener('visibilitychange', visibilityChanged);
      paneVisibilityChanged = null;
      clearInterval(refresh);
      relayStore.unwatchPane(agent);
      lease.stop();
      virtualRowObserver?.disconnect();
      if (virtualWindowFrame) cancelAnimationFrame(virtualWindowFrame);
      stopWakeLock();
    };
  });

  async function applyFrame(
    next: TerminalFrame,
    preserveLineEnds: boolean,
  ) {
    if (terminalLayoutSuspended) return;
    const renderColumnCap = resizeLayoutActive
      ? lease.columns
      : (resizeLayoutPending ? measuredPaneColumns() || 0 : 0);
    const layoutChanged = preserveLineEnds !== lastPreserveLineEnds
      || renderColumnCap !== lastRenderColumnCap;
    if (next.content === lastContent && next.format === lastFormat && !layoutChanged) return;
    const rendered = renderTerminalContent(
      next.content,
      next.format,
      {
        preserveLayout: true,
        preserveLineEnds,
        // Cap box-drawing rows at the width in force. A fixed-grid row is a run
        // of fixed-width cells with no wrap opportunity between them, so past
        // the cap it must render as plain text that can wrap instead. Zero means
        // "no cap", which only a relay that cannot lease is entitled to.
        maxFixedGridColumns: renderColumnCap,
      },
    );
    lastContent = next.content;
    if (rendered.display === displayed && rendered.html === renderedHtml
      && next.format === lastFormat && !layoutChanged) return;
    // Exactly at the bottom counts too: the scroll event that would say so may
    // not have run yet when this frame arrives.
    const frameStick = virtualStickToBottom || Boolean(terminalElement
      && terminalElement.scrollHeight - terminalElement.scrollTop - terminalElement.clientHeight <= 1);
    const stick = resizeSessionActive && pendingResizeStick !== null
      ? pendingResizeStick
      : layoutChanged && pendingLayoutStick !== null
        ? pendingLayoutStick
        : frameStick;
    const previousTop = terminalElement?.scrollTop || 0;
    let previousAnchor = stick
      ? null
      : pendingResizeAnchor || currentVirtualAnchor(previousTop);
    // Rows cropped from the front shift every index; keep the anchor on the
    // same row.
    const rowShift = renderedRowShift(renderedRows, rendered.rows);
    if (rowShift) wideGridOffsets.clear();
    if (previousAnchor && rowShift) {
      previousAnchor = { ...previousAnchor, index: Math.max(0, previousAnchor.index - rowShift) };
    }
    pendingResizeStick = null;
    pendingResizeAnchor = null;
    if (layoutChanged) pendingLayoutStick = null;
    virtualStickToBottom = stick;
    virtualScrollResetPending = Boolean(terminalElement);
    displayed = rendered.display;
    renderedHtml = rendered.html;
    renderedRows = rendered.rows;
    lastFormat = next.format;
    lastPreserveLineEnds = preserveLineEnds;
    lastRenderColumnCap = renderColumnCap;
    const nextTop = resetVirtualRows(
      stick ? Number.POSITIVE_INFINITY : previousTop,
      previousAnchor,
    );
    await tick();
    if (terminalLayoutSuspended) return;
    if (!terminalElement) {
      virtualScrollResetPending = false;
      return;
    }
    if (layoutChanged) terminalElement.scrollLeft = 0;
    if (stick) {
      terminalElement.scrollTop = terminalElement.scrollHeight;
      virtualStickToBottom = true;
    } else {
      terminalElement.scrollTop = nextTop;
      if (previousAnchor) {
        const row = terminalElement.querySelector<HTMLElement>(`[data-terminal-row="${matchingAnchorIndex(previousAnchor)}"]`);
        if (row) terminalElement.scrollTop += row.getBoundingClientRect().top
          - terminalElement.getBoundingClientRect().top + previousAnchor.offset;
      }
    }
    rememberVirtualScrollGeometry(terminalElement);
    virtualScrollResetPending = false;
    observeVirtualRows();
  }

  function terminalScreenOffset(): number {
    return terminalElement?.querySelector<HTMLElement>('.term-screen')?.offsetTop || 0;
  }

  function currentVirtualAnchor(scrollTop: number): VirtualTerminalAnchor | null {
    if (!Number.isFinite(scrollTop) || !virtualIndex.length) return null;
    if (terminalElement) {
      const viewport = terminalElement.getBoundingClientRect();
      const element = [...terminalElement.querySelectorAll<HTMLElement>('[data-terminal-row]')]
        .find((row) => {
          const bounds = row.getBoundingClientRect();
          return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
        });
      const index = Number.parseInt(element?.dataset.terminalRow || '', 10);
      if (element && Number.isInteger(index)) {
        return {
          index,
          offset: Math.max(0, viewport.top - element.getBoundingClientRect().top),
          text: renderedRows[index]?.text || '',
        };
      }
    }
    const contentTop = terminalScreenOffset();
    const index = virtualIndex.indexAt(Math.max(0, scrollTop - contentTop));
    return {
      index,
      offset: Math.max(0, scrollTop - contentTop - virtualIndex.offset(index)),
      text: renderedRows[index]?.text || '',
    };
  }

  function matchingAnchorIndex(anchor: VirtualTerminalAnchor): number {
    const fallback = Math.min(anchor.index, Math.max(0, renderedRows.length - 1));
    const target = anchor.text.trim();
    if (target.length < 4) return fallback;
    let bestIndex = fallback;
    let bestScore = 0;
    let bestDistance = Number.POSITIVE_INFINITY;
    for (let index = 0; index < renderedRows.length; index += 1) {
      const candidate = renderedRows[index].text.trim();
      const score = candidate === target
        ? 2
        : target.length >= 8 && (candidate.includes(target) || target.includes(candidate)) ? 1 : 0;
      const distance = Math.abs(index - anchor.index);
      if (score > bestScore || (score === bestScore && score > 0 && distance < bestDistance)) {
        bestIndex = index;
        bestScore = score;
        bestDistance = distance;
      }
    }
    return bestIndex;
  }

  function anchorOffsetLimit(anchor: VirtualTerminalAnchor, anchorIndex: number): number {
    const target = anchor.text.trim();
    let height = 0;
    let matches = 0;
    let lastSize = 0;
    for (let index = anchorIndex; index < renderedRows.length; index += 1) {
      const candidate = renderedRows[index].text.trim();
      if (!candidate || !target.includes(candidate)) break;
      lastSize = virtualIndex.size(index);
      height += lastSize;
      matches += 1;
      if (candidate === target) break;
    }
    return Math.max(0, matches > 1 ? height - lastSize : height - 1);
  }

  function resetVirtualRows(
    scrollTop: number,
    previousAnchor = currentVirtualAnchor(scrollTop),
  ) {
    const width = terminalElement?.clientWidth
      || Math.round(window.visualViewport?.width || window.innerWidth);
    const layoutSignature = [
      resizeLayoutActive ? 'resize' : 'fixed',
      lastPreserveLineEnds ? 'line-ends' : 'wrapped',
      lease.layoutColumns,
      $interfaceSize,
      width,
    ].join(':');
    if (layoutSignature !== virtualLayoutSignature) {
      virtualLayoutSignature = layoutSignature;
      virtualHeightCache.clear();
    } else if (virtualHeightCache.size > Math.max(2_000, renderedRows.length * 2)) {
      virtualHeightCache.clear();
    }

    const style = terminalElement ? getComputedStyle(terminalElement) : null;
    const parsedLineHeight = Number.parseFloat(style?.lineHeight || '');
    const lineHeight = Number.isFinite(parsedLineHeight) && parsedLineHeight > 0 ? parsedLineHeight : 18;
    const wrappingColumns = measuredPaneColumns()
      || lease.layoutColumns
      || 80;
    const sizes = renderedRows.map((row) => {
      const measured = virtualHeightCache.get(row.html);
      if (measured) return measured;
      if (row.separator) return lineHeight * 1.2;
      // One line per row only where the row really stays on one line: a leased
      // pane's fixed-grid rows. Everything else wraps at the width in force,
      // and estimating those at one line parks the scroll past the content.
      // Rows wrap in the two regimes that have a width to wrap at: engaged
      // (the leased width, fixed grids excepted) and pending (the container).
      // A relay that cannot lease keeps every row on one line.
      const wraps = ((resizeLayoutPending && !row.wideGrid)
        || (resizeLayoutActive && !row.fixedGrid && !row.wideGrid))
        ? Math.max(1, Math.ceil(row.columns / wrappingColumns))
        : 1;
      return lineHeight * wraps;
    });
    virtualIndex.reset(sizes);
    let nextTop = scrollTop;
    if (previousAnchor && virtualIndex.length) {
      const anchorIndex = matchingAnchorIndex(previousAnchor);
      const anchorOffset = Math.min(
        Math.max(0, previousAnchor.offset),
        anchorOffsetLimit(previousAnchor, anchorIndex),
      );
      nextTop = terminalScreenOffset() + virtualIndex.offset(anchorIndex) + anchorOffset;
    }

    if (resizeLayoutPending) virtualContentColumns = 0;
    else {
      virtualContentColumns = terminalScreenColumns(
        renderedRows,
        resizeLayoutActive,
        lease.layoutColumns,
      );
    }
    renderVirtualWindow(nextTop, true);
    return nextTop;
  }

  function mountedVirtualHtml(start: number, end: number): string {
    let html = '';
    for (let index = start; index < end; index += 1) {
      const attributes = renderedRows[index].wideGrid && wideGridBlocks[index] >= 0
        ? `<span data-terminal-row="${index}" data-terminal-wide-block="${wideGridBlocks[index]}" `
        : `<span data-terminal-row="${index}" `;
      html += renderedRows[index].html.replace('<span ', attributes);
    }
    return html;
  }

  // Contiguous wide box-drawn rows form one logical table. Their borders were
  // collapsed to separators, so a separator between two wide rows continues
  // the block instead of splitting the table into per-row scroll islands.
  const wideGridBlocks = $derived.by(() => {
    const blocks = new Array<number>(renderedRows.length).fill(-1);
    let block = -1;
    let open = false;
    for (let index = 0; index < renderedRows.length; index += 1) {
      if (renderedRows[index].wideGrid) {
        if (!open) {
          block += 1;
          open = true;
        }
        blocks[index] = block;
      } else if (!renderedRows[index].separator) {
        open = false;
      }
    }
    return blocks;
  });

  function syncWideGridScroll(event: Event) {
    const row = event.target;
    if (!(row instanceof HTMLElement) || row.dataset.terminalWideBlock === undefined || !terminalElement) return;
    const block = Number(row.dataset.terminalWideBlock);
    if (wideGridOffsets.get(block) === row.scrollLeft) return;
    wideGridOffsets.set(block, row.scrollLeft);
    for (const sibling of terminalElement.querySelectorAll<HTMLElement>(`[data-terminal-wide-block="${block}"]`)) {
      if (sibling !== row && sibling.scrollLeft !== row.scrollLeft) sibling.scrollLeft = row.scrollLeft;
    }
  }

  function restoreWideGridScroll() {
    if (!terminalElement) return;
    for (const row of terminalElement.querySelectorAll<HTMLElement>('[data-terminal-wide-block]')) {
      const offset = wideGridOffsets.get(Number(row.dataset.terminalWideBlock));
      if (offset && row.scrollLeft !== offset) row.scrollLeft = offset;
    }
  }

  function renderVirtualWindow(scrollTop: number, force = false) {
    const viewportHeight = terminalElement?.clientHeight || window.innerHeight;
    const viewportTop = Number.isFinite(scrollTop)
      ? scrollTop
      : Math.max(0, virtualIndex.total - viewportHeight);
    const range = virtualIndex.range(viewportTop, viewportHeight, viewportHeight * 1.5);
    const unchanged = range.start === virtualStart && range.end === virtualEnd;
    virtualStart = range.start;
    virtualEnd = range.end;
    virtualTopHeight = range.top;
    virtualBottomHeight = range.bottom;
    if (force || !unchanged) {
      virtualHtml = mountedVirtualHtml(range.start, range.end);
      queueVirtualRowObservation();
    }
  }

  function queueVirtualRowObservation() {
    void tick().then(observeVirtualRows);
  }

  function observeVirtualRows() {
    if (!terminalElement || terminalLayoutSuspended) return;
    restoreWideGridScroll();
    if (typeof ResizeObserver === 'undefined') return;
    virtualRowObserver ||= new ResizeObserver(measureVirtualRows);
    virtualRowObserver.disconnect();
    for (const row of terminalElement.querySelectorAll<HTMLElement>('[data-terminal-row]')) {
      virtualRowObserver.observe(row);
    }
  }

  function measureVirtualRows(entries: ResizeObserverEntry[]) {
    if (!terminalElement || !entries.length || terminalLayoutSuspended) return;
    if (virtualScrollResetPending) return;
    const previousTop = terminalElement.scrollTop;
    const wasAtBottom = virtualStickToBottom;
    const anchor = virtualIndex.indexAt(previousTop);
    let anchorDelta = 0;
    let changed = false;
    for (const entry of entries) {
      const element = entry.target as HTMLElement;
      const index = Number.parseInt(element.dataset.terminalRow || '', 10);
      if (!Number.isInteger(index) || index < 0 || index >= renderedRows.length) continue;
      const borderSize = Array.isArray(entry.borderBoxSize)
        ? entry.borderBoxSize[0]
        : entry.borderBoxSize;
      let height = borderSize?.blockSize || entry.contentRect.height;
      if (renderedRows[index].separator) {
        const style = getComputedStyle(element);
        height += (Number.parseFloat(style.marginTop) || 0)
          + (Number.parseFloat(style.marginBottom) || 0);
      }
      const delta = virtualIndex.update(index, height);
      if (!delta) continue;
      virtualHeightCache.set(renderedRows[index].html, height);
      if (index < anchor) anchorDelta += delta;
      changed = true;
    }
    if (!changed) return;
    const nextTop = wasAtBottom ? virtualIndex.total : previousTop + anchorDelta;
    virtualStickToBottom = wasAtBottom;
    virtualScrollResetPending = true;
    renderVirtualWindow(nextTop);
    void tick().then(() => {
      if (terminalLayoutSuspended) return;
      if (!terminalElement) {
        virtualScrollResetPending = false;
        return;
      }
      terminalElement.scrollTop = wasAtBottom ? terminalElement.scrollHeight : nextTop;
      rememberVirtualScrollGeometry(terminalElement);
      if (wasAtBottom) virtualStickToBottom = true;
      virtualScrollResetPending = false;
    });
  }

  function scheduleVirtualWindow() {
    if (terminalLayoutSuspended || virtualWindowFrame) return;
    virtualWindowFrame = requestAnimationFrame(() => {
      virtualWindowFrame = 0;
      if (terminalElement && !terminalLayoutSuspended) renderVirtualWindow(terminalElement.scrollTop);
    });
  }

  // The header owns the Find control, so the terminal exposes opening it rather
  // than lifting findOpen out: kept here, the bar still closes with the pane.
  export function openFind() {
    find?.open();
  }

  function scrollToRow(row: number) {
    const rowTop = terminalScreenOffset() + virtualIndex.offset(row);
    const nextTop = Math.max(0, rowTop - (terminalElement.clientHeight - virtualIndex.size(row)) / 2);
    virtualStickToBottom = false;
    virtualScrollResetPending = true;
    renderVirtualWindow(nextTop, true);
    return tick().then(() => {
      if (!terminalElement) return;
      terminalElement.scrollTop = nextTop;
      rememberVirtualScrollGeometry(terminalElement);
      virtualStickToBottom = terminalElement.scrollHeight
        - terminalElement.scrollTop
        - terminalElement.clientHeight < 48;
      virtualScrollResetPending = false;
    });
  }

  function jumpToBottom() {
    if (terminalLayoutSuspended) return;
    virtualStickToBottom = true;
    virtualScrollResetPending = true;
    renderVirtualWindow(Number.POSITIVE_INFINITY, true);
    void tick().then(() => {
      if (terminalLayoutSuspended) return;
      if (!terminalElement) {
        virtualScrollResetPending = false;
        return;
      }
      terminalElement.scrollTop = terminalElement.scrollHeight;
      rememberVirtualScrollGeometry(terminalElement);
      virtualScrollResetPending = false;
    });
  }

  function handleScroll() {
    // A scroll event queued during teardown can fire after Svelte has already
    // cleared the bind:this reference; there is nothing left to measure.
    if (!terminalElement || terminalLayoutSuspended) return;
    if (virtualScrollResetPending) {
      rememberVirtualScrollGeometry(terminalElement);
      return;
    }
    const scrollTop = terminalElement.scrollTop;
    const scrollHeight = terminalElement.scrollHeight;
    const clientHeight = terminalElement.clientHeight;
    const bottomDistance = scrollHeight - scrollTop - clientHeight;
    // Only a viewport/controls height change may re-pin: content growth also
    // changes scrollHeight, and a user scrolling up during a stream must win.
    const layoutChanged = Math.abs(clientHeight - virtualClientHeight) >= 1;
    // A scroll that lands exactly at the bottom is never the user moving
    // toward history: when corrected row heights shrink the content while
    // pinned, the browser clamps scrollTop to the new maximum and fires a
    // scroll event whose position is lower than the remembered one. Reading
    // that clamp as intent dropped stick-to-bottom, so Safari — whose real
    // row heights disagree with the estimates more than Chromium's — opened
    // a growing gap above the transcript's end and fought every scroll with
    // anchor-preserving corrections (issue #11's missing bottom + flicker).
    const movedTowardHistory = !layoutChanged
      && scrollTop < virtualScrollTop - 1
      && bottomDistance > 1;
    rememberVirtualScrollGeometry(terminalElement);
    if (movedTowardHistory) {
      virtualStickToBottom = false;
    } else if (bottomDistance <= 1) {
      virtualStickToBottom = true;
    } else if (virtualStickToBottom) {
      jumpToBottom();
      return;
    }
    scheduleVirtualWindow();
  }

  function paneSizeLeaseSupported(target: Agent): boolean {
    const connection = $connections.get(target.relay_id);
    return resizeSessionActive && !questionMode && connection?.status === 'connected';
  }

  function paneSizeRowLeaseSupported(target: Agent): boolean {
    return $terminalHeightLease && Boolean(
      $connections.get(target.relay_id)?.capabilities.includes('pane_size_opt_in_rows'),
    );
  }

  // A toggle flips the next lease immediately: on adds the measured rows, off
  // renews width-only, which lifts this client's row constraint on the relay.
  $effect(() => {
    const enabled = $terminalHeightLease;
    void enabled;
    void tick().then(() => lease.request(false));
  });

  function measuredPaneColumns(): number | null {
    if (!terminalElement || !cellMeasureElement) return null;
    const cellWidth = cellMeasureElement.getBoundingClientRect().width / CELL_MEASURE_TEXT.length;
    const style = getComputedStyle(terminalElement);
    const horizontalPadding = (Number.parseFloat(style.paddingLeft) || 0)
      + (Number.parseFloat(style.paddingRight) || 0);
    const usableWidth = terminalElement.clientWidth - horizontalPadding;
    if (!Number.isFinite(cellWidth) || cellWidth <= 0 || usableWidth <= 0) return null;
    // The probed advance also drives every CSS width cap, so the caps stay
    // correct the moment the font or interface size changes.
    if (cellWidth !== measuredCellWidth) measuredCellWidth = cellWidth;
    return Math.min(
      MAX_PANE_SIZE_COLUMNS,
      Math.max(MIN_PANE_SIZE_COLUMNS, Math.floor(usableWidth / cellWidth)),
    );
  }

  // 0 means "do not lease rows": the relay keeps the pane's own height.
  function measuredPaneRows(): number {
    if (!terminalElement) return 0;
    const style = getComputedStyle(terminalElement);
    const lineHeight = Number.parseFloat(style.lineHeight);
    const verticalPadding = (Number.parseFloat(style.paddingTop) || 0)
      + (Number.parseFloat(style.paddingBottom) || 0);
    const usableHeight = terminalElement.clientHeight - verticalPadding;
    if (!Number.isFinite(lineHeight) || lineHeight <= 0 || usableHeight <= 0) return 0;
    return Math.min(
      MAX_PANE_SIZE_ROWS,
      Math.max(MIN_PANE_SIZE_ROWS, Math.floor(usableHeight / lineHeight)),
    );
  }

  function menuKeyLabel(keys: string[]): string {
    return keys.map((key) => key === ' ' ? 'Space' : key).join('+');
  }

  function openNext() {
    if (nextBlocked) {
      replaceView({
        view: 'terminal',
        paneId: nextBlocked.pane_id,
        target: targetRefForAgent(nextBlocked) || undefined,
      });
    }
  }
</script>

{#snippet manualInputTrigger()}
  {#if manualInputOffered}
    <div class="manual-input-trigger" bind:this={manualInputTriggerHost}>
      <Button variant="secondary" size="sm" aria-label="Keyboard" title="Type into the terminal" disabled={manualInputBusy || manualInputLoading}
        onclick={() => { void terminalInputMode?.enter(); }}><KeyboardIcon /></Button>
    </div>
  {/if}
{/snippet}

<main
  class:question-only={questionMode && !manualInput}
  class:find-open={findOpen}
  class="terminal-view"
  aria-label={`${questionMode ? 'Questions' : 'Terminal'} for ${agent.project || agent.name || agent.agent || 'agent'}`}
>
  {#if readOnly}
    <p class="key-feedback" role="status">Reader access is read only. Use a controller device to send input or answer prompts.</p>
  {/if}
  {#if questionMode && interaction && !manualInput}
    <AgentInteraction {agent} {readOnly} responding={responding.has(agent.pane_id)} blocked={inputBlocked} />
    <TerminalKeys queue={keyQueue} {agent} {readOnly} blocked={inputBlocked} bind:sending={keySending} extra={manualInputTrigger} />
  {/if}
  <div class:hidden={questionMode} class="terminal-view term">
  <TerminalFind
    bind:this={find}
    bind:open={findOpen}
    rows={renderedRows}
    terminal={terminalElement}
    mountedRange={() => ({ start: virtualStart, end: virtualEnd })}
    domVersion={virtualHtml}
    disabled={questionMode}
    {scrollToRow}
  />
  <div class="term-wrap">
  <div
    class:resize-layout={resizeLayoutActive} class:resize-pending={resizeLayoutPending}
    class="term-content"
    style={terminalContentStyle}
    bind:this={terminalElement}
    role="log"
    aria-label="Agent terminal output"
    onscroll={handleScroll}
    onscrollcapture={syncWideGridScroll}
  >
    <span
      bind:this={cellMeasureElement}
      aria-hidden="true"
      style="pointer-events: none; position: absolute; visibility: hidden; white-space: pre;"
    >{CELL_MEASURE_TEXT}</span>
    <div class="term-screen" data-terminal-row-count={renderedRows.length}>
      {#if virtualTopHeight > 0}
        <span class="terminal-virtual-spacer" style={`height:${virtualTopHeight}px`} aria-hidden="true"></span>
      {/if}
      <!-- Normalized rows are escaped before controlled ANSI spans enter this bounded DOM window. -->
      {@html virtualHtml}
      {#if virtualBottomHeight > 0}
        <span class="terminal-virtual-spacer" style={`height:${virtualBottomHeight}px`} aria-hidden="true"></span>
      {/if}
    </div>
  </div>
  </div>
  <TerminalResponseActions {agent} {readOnly} {frame} plainText={terminalPlainText} busy={responding.has(agent.pane_id)} />

  </div>
  <div class="terminal-bottom">
    <TerminalInputMode bind:this={terminalInputMode} {agent} {frame} {readOnly} bind:manual={manualInput}
      bind:loading={manualInputLoading} busy={manualInputBusy} returnFocus={() => manualInputTriggerHost?.querySelector('button')?.focus()} />
    <div hidden={manualInput}>
    {#if !questionMode && !manualInput}<TerminalKeys queue={keyQueue} bind:this={terminalKeys} {agent} {readOnly} blocked={sendingFilter || inputBlocked} bind:sending={keySending} extra={manualInputTrigger} />{/if}
    {#key JSON.stringify(targetRefForAgent(agent))}
      <PromptComposer {agent} {frame} {readOnly} dispatchBlocked={manualInput ? 'Check terminal input delivery before sending.' : dispatchBlocked}
        blockExplained={manualInput} attachmentsBlocked={inputLocked}
        {submitAdapter} bind:sending={sendingPrompt}
        placeholder={terminalTextMode ? 'Type filter text…' : 'Type a reply…'}
        submitLabel={terminalTextMode ? 'Send filter text' : 'Send prompt'} />
    {/key}
    {#if lease.error}<p class="upload-status error" role="alert">{lease.error}</p>{/if}
    {#if historyTruncated}
      <p class="upload-status" role="status">Older terminal history is not shown; this pane response was limited.</p>
    {/if}

    {#if visibleTerminalMenu}
      <section class="generic-menu-actions" aria-label={`Terminal menu: ${visibleTerminalMenu.title}`} aria-busy={keySending}>
        <header>
          <strong>{visibleTerminalMenu.title}</strong>
          <span>Detected from terminal key hints</span>
          <button type="button" aria-label="Dismiss detected menu actions" onclick={() => { dismissedMenuSignature = visibleTerminalMenu.signature; }}>×</button>
        </header>
        <div>
          {#each visibleTerminalMenu.actions as action (action.keys.join('+'))}
            <Button
              variant={action.cancel ? 'secondary' : 'default'}
              size="sm"
              disabled={readOnly || keySending || inputBlocked}
              onclick={() => { void terminalKeys?.sendKeys(action.keys, action.label); }}
            ><kbd>{menuKeyLabel(action.keys)}</kbd>{action.label}</Button>
          {/each}
        </div>
      </section>
    {/if}

    {#if !questionMode && !manualInput}<AgentInteraction {agent} {readOnly} responding={responding.has(agent.pane_id)} blocked={inputBlocked} />{/if}
    {#if nextBlocked}
      <div class="quick-actions"><Button variant="secondary" onclick={openNext}>Next blocked →</Button></div>
    {/if}
    </div>
  </div>
</main>

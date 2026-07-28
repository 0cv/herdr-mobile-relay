<script lang="ts" module>
  import type { TerminalFrame as CachedTerminalFrame } from '$lib/types';
  import type { RenderedTerminalRow } from '$lib/terminal';

  interface ResizedTerminalFrame {
    frame: CachedTerminalFrame;
    columns: number;
    display: string;
    html: string;
    rows: RenderedTerminalRow[];
    historyLines: number;
    interfaceSize: string;
    viewportWidth: number;
  }

  const MAX_RESIZED_TERMINAL_FRAMES = 8;
  const resizedTerminalFrames = new Map<string, ResizedTerminalFrame>();

  function rememberResizedTerminalFrame(paneId: string, cached: ResizedTerminalFrame) {
    resizedTerminalFrames.delete(paneId);
    resizedTerminalFrames.set(paneId, cached);
    if (resizedTerminalFrames.size <= MAX_RESIZED_TERMINAL_FRAMES) return;
    const oldestPaneId = resizedTerminalFrames.keys().next().value;
    if (oldestPaneId) resizedTerminalFrames.delete(oldestPaneId);
  }
</script>

<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import QuestionForm from '$components/QuestionForm.svelte';
  import {
    MAX_PANE_SIZE_COLUMNS,
    MIN_PANE_SIZE_COLUMNS,
  } from '$lib/config';
  import {
    agentNeedsInspection,
    agentNeedsResponse,
    attentionKind,
    approvalButtonTone,
    approvalOptions,
    questionInteraction,
    sortedAgents,
  } from '$lib/agents';
  import {
    interfaceSize,
    terminalHistoryLines,
    terminalLayout,
  } from '$lib/preferences';
  import { replaceView } from '$lib/router';
  import { relayStore, CommandError } from '$lib/store';
  import { stripAnsi, TERMINAL_SEPARATOR_TOKEN, renderTerminalContent } from '$lib/terminal';
  import type { Agent, SlashCommand, SlashCommandCatalog, TerminalFrame } from '$lib/types';
  import { VirtualTerminalIndex } from '$lib/virtual-terminal';

  const connections = relayStore.connections;

  let {
    agent,
    allAgents,
    frame,
    responding,
  }: {
    agent: Agent;
    allAgents: Agent[];
    frame?: TerminalFrame;
    responding: Set<string>;
  } = $props();

  interface VirtualTerminalAnchor {
    index: number;
    offset: number;
    text: string;
  }

  let terminalElement = $state<HTMLDivElement>(null!);
  let cellMeasureElement = $state<HTMLSpanElement>(null!);
  let fileInput = $state<HTMLInputElement>(null!);
  let ctrlInputElement = $state<HTMLInputElement>(null!);
  let composerElement = $state<HTMLTextAreaElement>(null!);
  let transcriptElement = $state<HTMLTextAreaElement>(null!);
  let composer = $state('');
  let composerFocused = $state(false);
  let deferredFrame: TerminalFrame | undefined;
  let resizeFrameBaseline: TerminalFrame | undefined;
  let showingCachedResizeFrame = false;
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
  let pendingResizeAnchor: VirtualTerminalAnchor | null = null;
  let pendingResizeStick: boolean | null = null;
  let pendingLayoutStick: boolean | null = null;
  let virtualWindowFrame = 0;
  let virtualRowObserver: ResizeObserver | undefined;
  let virtualHeightCache = new Map<string, number>();
  const virtualIndex = new VirtualTerminalIndex();
  let lastFormat = '';
  let lastContent = '';
  let lastPreserveLayout = false;
  let lastPreserveLineEnds = false;
  let jumpVisible = $state(false);
  let arrowsOpen = $state(false);
  let ctrlArmed = $state(false);
  let uploadStatus = $state('');
  let uploadError = $state(false);
  let paneSizeLeaseError = $state('');
  let requestedPaneId = '';
  let slashCatalog = $state<SlashCommandCatalog>({ commands: [], truncated: false });
  let slashCatalogLoading = $state(true);
  let slashCatalogUnavailable = $state(false);
  let activeSlashIndex = $state(0);
  let dismissedSlashQuery = $state<string | null>(null);
  const CELL_MEASURE_TEXT = '0000000000';
  const PANE_SIZE_LEASE_REFRESH_MS = 10_000;
  const PANE_SIZE_SETTLE_MS = 250;
  const PANE_SIZE_SETTLE_TIMEOUT_MS = 1_500;
  const PANE_SIZE_SETTLE_MIN_HISTORY = 100;
  let componentMounted = false;
  let leaseGeneration = 0;
  let leaseInFlight = false;
  let leaseTarget: Agent | null = null;
  let lastLeasedColumns = $state(0);
  let renderedResizeColumns = $state(0);
  let queuedLease: { columns: number; force: boolean } | null = null;
  let resizeReadPending = $state(false);
  let resizeExpectedLines = 0;
  let resizeSettleDeadline = 0;
  let resizeReadTimer: ReturnType<typeof setTimeout> | undefined;

  const responsePending = $derived(agentNeedsResponse(agent));
  const approvalMode = $derived(responsePending && attentionKind(agent) === 'approval');
  const inspectionMode = $derived(agentNeedsInspection(agent));
  const inputLocked = $derived(responsePending || inspectionMode);
  const interaction = $derived(questionInteraction(agent));
  const questionMode = $derived(Boolean(responsePending && attentionKind(agent) === 'question' && interaction));
  const resizeLayout = $derived($terminalLayout === 'resize');
  const resizeSessionActive = $derived(resizeLayout
    && Boolean($connections.get(agent.relay_id)?.capabilities.includes('pane_size_lease')));
  const preserveLayout = $derived($terminalLayout !== 'readable');
  const options = $derived(approvalOptions(agent));
  const nextBlocked = $derived(sortedAgents(allAgents.filter((item) => agentNeedsResponse(item) && item.pane_id !== agent.pane_id))[0]);
  const slashQuery = $derived(composer.startsWith('/') && !/\s/.test(composer) ? composer.slice(1).toLocaleLowerCase() : null);
  const filteredSlashCommands = $derived.by(() => {
    if (slashQuery === null) return [];
    if (!slashQuery) return slashCatalog.commands;
    return slashCatalog.commands.filter((entry) => entry.command.slice(1).toLocaleLowerCase().startsWith(slashQuery));
  });
  const effectiveSlashIndex = $derived(filteredSlashCommands.length
    ? Math.min(activeSlashIndex, filteredSlashCommands.length - 1)
    : -1);
  const slashMenuOpen = $derived(!inputLocked
    && !questionMode
    && slashQuery !== null
    && dismissedSlashQuery !== composer);
  const terminalPlainText = $derived(
    stripAnsi(displayed).replaceAll(TERMINAL_SEPARATOR_TOKEN, '────────'),
  );
  const terminalContentStyle = $derived.by(() => {
    const styles: string[] = [];
    if (resizeSessionActive && (lastLeasedColumns || renderedResizeColumns)) {
      styles.push(`--terminal-width: ${lastLeasedColumns || renderedResizeColumns}ch`);
    }
    if (virtualContentColumns > 0) {
      styles.push(`--terminal-content-width: ${virtualContentColumns}ch`);
    }
    return styles.length ? styles.join(';') : undefined;
  });

  $effect(() => {
    const next = frame;
    const preserve = preserveLayout;
    const preserveLineEnds = preserve && !resizeSessionActive;
    const cachedResizeFrame = validResizedTerminalFrame(next);
    if (resizeSessionActive
      && cachedResizeFrame
      && (lastLeasedColumns === 0
        || resizeReadPending
        || next === resizeFrameBaseline)) {
      renderedResizeColumns = cachedResizeFrame.columns;
      showingCachedResizeFrame = true;
      const cachedLayoutChanged = !lastPreserveLayout || lastPreserveLineEnds;
      if (untrack(() => composerFocused) && !cachedLayoutChanged) {
        deferredFrame = cachedResizeFrame.frame;
      } else if (cachedLayoutChanged || !lastContent) {
        untrack(() => { void applyCachedResizeFrame(cachedResizeFrame); });
      }
      return;
    }
    const incompleteResizeHistory = resizeSessionActive
      && !resizeReadPending
      && resizeExpectedLines >= PANE_SIZE_SETTLE_MIN_HISTORY
      && next !== resizeFrameBaseline
      && terminalFrameLineCount(next) * 2 < resizeExpectedLines
      && Date.now() < resizeSettleDeadline;
    if (incompleteResizeHistory) scheduleSettledPaneRead(agent, leaseGeneration);
    const waitingForResizedFrame = resizeSessionActive
      && !paneSizeLeaseError
      && (lastLeasedColumns === 0
        || resizeReadPending
        || next === resizeFrameBaseline
        || incompleteResizeHistory);
    if (waitingForResizedFrame && showingCachedResizeFrame) return;
    if (!next || next.paneId !== agent.pane_id || waitingForResizedFrame) {
      untrack(() => {
        if (waitingForResizedFrame && pendingResizeStick === null) {
          pendingResizeStick = virtualStickToBottom;
          pendingResizeAnchor = virtualStickToBottom
            ? null
            : currentVirtualAnchor(terminalElement?.scrollTop || 0);
        } else if (!waitingForResizedFrame) {
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
        deferredFrame = undefined;
        jumpVisible = false;
      });
      return;
    }
    showingCachedResizeFrame = false;
    if (resizeSessionActive && lastLeasedColumns > 0) renderedResizeColumns = lastLeasedColumns;
    resizeFrameBaseline = undefined;
    resizeExpectedLines = 0;
    resizeSettleDeadline = 0;
    const layoutChanged = preserve !== lastPreserveLayout
      || preserveLineEnds !== lastPreserveLineEnds;
    if (untrack(() => composerFocused) && !layoutChanged) deferredFrame = next;
    else untrack(() => { void applyFrame(next, preserve, preserveLineEnds); });
  });

  $effect(() => {
    const paneId = agent.pane_id;
    const connected = $connections.get(agent.relay_id)?.status === 'connected';
    if (!connected) {
      requestedPaneId = '';
      return;
    }
    if (resizeSessionActive && !paneSizeLeaseError) return;
    if (paneId === requestedPaneId) return;
    requestedPaneId = paneId;
    relayStore.readPane(agent);
  });

  $effect(() => {
    const connection = $connections.get(agent.relay_id);
    const interfaceSizeValue = $interfaceSize;
    const paneId = agent.pane_id;
    void interfaceSizeValue;
    if (!resizeLayout || questionMode) {
      releasePaneSizeLease(componentMounted);
      paneSizeLeaseError = '';
      return;
    }
    if (connection?.status !== 'connected') {
      discardPaneSizeLease();
      paneSizeLeaseError = '';
      return;
    }
    if (!connection.capabilities.includes('pane_size_lease')) {
      discardPaneSizeLease();
      paneSizeLeaseError = 'Resize Session is unavailable on this relay. Original Columns rendering remains active.';
      return;
    }
    if (leaseTarget && leaseTarget.pane_id !== paneId) releasePaneSizeLease(componentMounted);
    paneSizeLeaseError = '';
    void tick().then(() => requestPaneSizeLease(false));
  });
  $effect.pre(() => {
    const layout = $terminalLayout;
    const interfaceSizeValue = $interfaceSize;
    void layout;
    void interfaceSizeValue;
    if (!terminalElement) return;
    virtualStickToBottom = terminalElement.scrollHeight
      - terminalElement.scrollTop
      - terminalElement.clientHeight < 48;
    pendingLayoutStick = virtualStickToBottom;
  });


  $effect(() => {
    const element = terminalElement;
    const interfaceSizeValue = $interfaceSize;
    void interfaceSizeValue;
    if (!element || typeof ResizeObserver === 'undefined') return;
    let previousWidth = element.clientWidth;
    untrack(() => {
      if (!renderedRows.length) return;
      const stick = virtualStickToBottom;
      virtualScrollResetPending = true;
      virtualLayoutSignature = '';
      const nextTop = resetVirtualRows(stick ? Number.POSITIVE_INFINITY : element.scrollTop);
      void tick().then(() => {
        element.scrollTop = stick ? element.scrollHeight : nextTop;
        if (stick) virtualStickToBottom = true;
        virtualScrollResetPending = false;
      });
    });
    const observer = new ResizeObserver(() => {
      const nextWidth = element.clientWidth;
      if (renderedRows.length && Math.abs(nextWidth - previousWidth) >= 1) {
        const stick = element.scrollHeight - element.scrollTop - element.clientHeight < 48;
        virtualStickToBottom = stick;
        virtualScrollResetPending = true;
        previousWidth = nextWidth;
        virtualLayoutSignature = '';
        const nextTop = resetVirtualRows(stick ? Number.POSITIVE_INFINITY : element.scrollTop);
        void tick().then(() => {
          element.scrollTop = stick ? element.scrollHeight : nextTop;
          if (stick) virtualStickToBottom = true;
          virtualScrollResetPending = false;
        });
      } else scheduleVirtualWindow();
      requestPaneSizeLease(false);
    });
    observer.observe(element);
    return () => observer.disconnect();
  });

  $effect(() => {
    if (!slashMenuOpen || effectiveSlashIndex < 0) return;
    const optionId = `slash-command-option-${effectiveSlashIndex}`;
    void tick().then(() => document.getElementById(optionId)?.scrollIntoView?.({ block: 'nearest' }));
  });

  $effect(() => {
    const resizeFor = [composer, $interfaceSize];
    void tick().then(() => {
      if (resizeFor[0] === composer && resizeFor[1] === $interfaceSize) resizeComposer();
    });
  });

  onMount(() => {
    let mounted = true;
    componentMounted = true;
    void relayStore.loadSlashCommands(agent).then((catalog) => {
      if (!mounted) return;
      slashCatalog = catalog;
      slashCatalogUnavailable = false;
    }).catch(() => {
      if (mounted) slashCatalogUnavailable = true;
    }).finally(() => {
      if (mounted) slashCatalogLoading = false;
    });
    const measurePane = () => requestPaneSizeLease(false);
    window.addEventListener('resize', measurePane);
    window.visualViewport?.addEventListener('resize', measurePane);
    const refresh = setInterval(() => relayStore.readPane(agent), 3_000);
    const refreshPaneSizeLease = setInterval(
      () => requestPaneSizeLease(true),
      PANE_SIZE_LEASE_REFRESH_MS,
    );
    void tick().then(measurePane);
    return () => {
      mounted = false;
      componentMounted = false;
      window.removeEventListener('resize', measurePane);
      window.visualViewport?.removeEventListener('resize', measurePane);
      clearInterval(refresh);
      clearInterval(refreshPaneSizeLease);
      releasePaneSizeLease(false);
      virtualRowObserver?.disconnect();
      if (virtualWindowFrame) cancelAnimationFrame(virtualWindowFrame);
    };
  });

  async function applyFrame(
    next: TerminalFrame,
    preserve = preserveLayout,
    preserveLineEnds = preserve && !resizeSessionActive,
  ) {
    const layoutChanged = preserve !== lastPreserveLayout
      || preserveLineEnds !== lastPreserveLineEnds;
    if (next.content === lastContent && next.format === lastFormat && !layoutChanged) {
      rememberCurrentResizeFrame(next, displayed, renderedHtml, renderedRows);
      return;
    }
    const rendered = renderTerminalContent(next.content, next.format, preserve, preserveLineEnds);
    lastContent = next.content;
    if (rendered.display === displayed && rendered.html === renderedHtml
      && next.format === lastFormat && !layoutChanged) {
      rememberCurrentResizeFrame(next, rendered.display, rendered.html, rendered.rows);
      return;
    }
    const frameStick = terminalElement
      ? terminalElement.scrollHeight - terminalElement.scrollTop - terminalElement.clientHeight < 48
      : virtualStickToBottom;
    const stick = resizeSessionActive && pendingResizeStick !== null
      ? pendingResizeStick
      : layoutChanged && pendingLayoutStick !== null
        ? pendingLayoutStick
        : frameStick;
    const previousTop = terminalElement?.scrollTop || 0;
    const previousAnchor = stick
      ? null
      : pendingResizeAnchor || currentVirtualAnchor(previousTop);
    pendingResizeStick = null;
    pendingResizeAnchor = null;
    if (layoutChanged) pendingLayoutStick = null;
    virtualStickToBottom = stick;
    virtualScrollResetPending = Boolean(terminalElement);
    displayed = rendered.display;
    renderedHtml = rendered.html;
    renderedRows = rendered.rows;
    lastFormat = next.format;
    lastPreserveLayout = preserve;
    lastPreserveLineEnds = preserveLineEnds;
    const nextTop = resetVirtualRows(
      stick ? Number.POSITIVE_INFINITY : previousTop,
      previousAnchor,
    );
    rememberCurrentResizeFrame(next, rendered.display, rendered.html, rendered.rows);
    await tick();
    if (!terminalElement) {
      virtualScrollResetPending = false;
      return;
    }
    if (layoutChanged) terminalElement.scrollLeft = 0;
    if (stick) {
      terminalElement.scrollTop = terminalElement.scrollHeight;
      jumpVisible = false;
      virtualStickToBottom = true;
    } else {
      terminalElement.scrollTop = nextTop;
      jumpVisible = true;
    }
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
    const width = terminalElement?.clientWidth || Math.round(currentViewportWidth());
    const layoutSignature = [
      lastPreserveLayout ? 'preserve' : 'readable',
      resizeSessionActive ? 'resize' : 'fixed',
      lastPreserveLineEnds ? 'line-ends' : 'wrapped',
      lastLeasedColumns || renderedResizeColumns,
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
      || lastLeasedColumns
      || renderedResizeColumns
      || 80;
    const sizes = renderedRows.map((row) => {
      const measured = virtualHeightCache.get(row.html);
      if (measured) return measured;
      if (row.separator) return lineHeight * 1.2;
      const wraps = (!lastPreserveLayout || (resizeSessionActive && !row.fixedGrid))
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

    if (!lastPreserveLayout) virtualContentColumns = 0;
    else {
      let columns = resizeSessionActive ? (lastLeasedColumns || renderedResizeColumns) : 0;
      for (const row of renderedRows) {
        if (!resizeSessionActive || row.fixedGrid) columns = Math.max(columns, row.columns);
      }
      virtualContentColumns = columns;
    }
    renderVirtualWindow(nextTop, true);
    return nextTop;
  }

  function mountedVirtualHtml(start: number, end: number): string {
    let html = '';
    for (let index = start; index < end; index += 1) {
      html += renderedRows[index].html.replace(
        '<span ',
        `<span data-terminal-row="${index}" `,
      );
    }
    return html;
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
    if (!terminalElement || typeof ResizeObserver === 'undefined') return;
    virtualRowObserver ||= new ResizeObserver(measureVirtualRows);
    virtualRowObserver.disconnect();
    for (const row of terminalElement.querySelectorAll<HTMLElement>('[data-terminal-row]')) {
      virtualRowObserver.observe(row);
    }
  }

  function measureVirtualRows(entries: ResizeObserverEntry[]) {
    if (!terminalElement || !entries.length) return;
    if (virtualScrollResetPending) return;
    const previousTop = terminalElement.scrollTop;
    const wasAtBottom = virtualStickToBottom
      || terminalElement.scrollHeight - previousTop - terminalElement.clientHeight < 48;
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
      if (!terminalElement) {
        virtualScrollResetPending = false;
        return;
      }
      terminalElement.scrollTop = wasAtBottom ? terminalElement.scrollHeight : nextTop;
      if (wasAtBottom) virtualStickToBottom = true;
      virtualScrollResetPending = false;
    });
  }

  function scheduleVirtualWindow() {
    if (virtualWindowFrame) return;
    virtualWindowFrame = requestAnimationFrame(() => {
      virtualWindowFrame = 0;
      if (terminalElement) renderVirtualWindow(terminalElement.scrollTop);
    });
  }

  function focusComposer(event: FocusEvent) {
    const target = event.target;
    if (!(target instanceof HTMLTextAreaElement)
      && !(target instanceof HTMLInputElement && target.classList.contains('question-other-input'))) return;
    composerFocused = true;
  }

  function blurComposer() {
    setTimeout(() => {
      const active = document.activeElement;
      if (active instanceof HTMLTextAreaElement
        || (active instanceof HTMLInputElement && active.classList.contains('question-other-input'))) return;
      composerFocused = false;
      const pending = deferredFrame;
      deferredFrame = undefined;
      if (pending) void applyFrame(pending);
    });
  }


  async function sendPrompt() {
    const text = composer.replace(/[\r\n]+$/g, '');
    if (!text || inputLocked) return;
    composer = '';
    try {
      await relayStore.sendToAgent(agent, { type: 'submit_prompt', text });
      relayStore.showToast('Prompt sent.');
    } catch (error) {
      if (!composer && !(error as CommandError).data?.dispatched_unknown) composer = text;
      relayStore.showToast((error as Error).message, true);
    }
    setTimeout(() => relayStore.readPane(agent), 500);
  }

  function composerInput() {
    if (dismissedSlashQuery !== composer) dismissedSlashQuery = null;
    activeSlashIndex = 0;
  }

  function clearComposer() {
    composer = '';
    dismissedSlashQuery = null;
    activeSlashIndex = 0;
  }

  function resizeComposer() {
    if (!composerElement) return;
    composerElement.style.height = 'auto';
    const maxHeight = Number.parseFloat(getComputedStyle(composerElement).maxHeight);
    const contentHeight = composerElement.scrollHeight;
    const capped = Number.isFinite(maxHeight) && contentHeight > maxHeight;
    composerElement.style.height = `${capped ? maxHeight : contentHeight}px`;
    composerElement.style.overflowY = capped ? 'auto' : 'hidden';
  }

  async function selectSlashCommand(command: SlashCommand) {
    composer = `${command.command}${command.argument_hint ? ' ' : ''}`;
    dismissedSlashQuery = composer;
    activeSlashIndex = 0;
    await tick();
    composerElement.focus();
    composerElement.setSelectionRange(composer.length, composer.length);
  }

  function keydown(event: KeyboardEvent) {
    if (event.isComposing) return;
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void sendPrompt();
      return;
    }
    if (!slashMenuOpen) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      dismissedSlashQuery = composer;
      return;
    }
    if (event.key === 'ArrowDown' && filteredSlashCommands.length) {
      event.preventDefault();
      activeSlashIndex = effectiveSlashIndex >= filteredSlashCommands.length - 1 ? 0 : effectiveSlashIndex + 1;
      return;
    }
    if (event.key === 'ArrowUp' && filteredSlashCommands.length) {
      event.preventDefault();
      activeSlashIndex = effectiveSlashIndex <= 0 ? filteredSlashCommands.length - 1 : effectiveSlashIndex - 1;
      return;
    }
    if ((event.key === 'Enter' || event.key === 'Tab') && effectiveSlashIndex >= 0) {
      event.preventDefault();
      void selectSlashCommand(filteredSlashCommands[effectiveSlashIndex]);
    }
  }

  async function sendKeys(keys: string[], activityLabel = ''): Promise<boolean> {
    try {
      await relayStore.sendToAgent(agent, { type: 'send_keys', keys, activity_label: activityLabel });
      setTimeout(() => relayStore.readPane(agent), 300);
      return true;
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
      return false;
    }
  }

  async function copyTerminalOutput() {
    const text = terminalPlainText;
    if (!text.trim()) return;
    if (!navigator.clipboard?.writeText) {
      transcriptElement.focus({ preventScroll: true });
      transcriptElement.select();
      relayStore.showToast('Terminal output selected. Use your browser Copy command.');
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      relayStore.showToast('Terminal output copied.');
    } catch {
      transcriptElement.focus({ preventScroll: true });
      transcriptElement.select();
      relayStore.showToast('Terminal output selected. Use your browser Copy command.');
    }
  }

  function toggleCtrl() {
    arrowsOpen = false;
    if (ctrlArmed) {
      ctrlArmed = false;
      ctrlInputElement.blur();
      return;
    }
    ctrlArmed = true;
    ctrlInputElement.value = '';
    ctrlInputElement.focus();
  }

  function ctrlInput(event: Event) {
    const target = event.currentTarget as HTMLInputElement;
    const letter = target.value.match(/[a-z]/i)?.[0];
    target.value = '';
    if (!letter) return;
    ctrlArmed = false;
    target.blur();
    const chord = `ctrl+${letter.toLowerCase()}`;
    void sendKeys([chord], `Ctrl+${letter.toUpperCase()}`);
  }

  function ctrlKeydown(event: KeyboardEvent) {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    ctrlArmed = false;
    ctrlInputElement.blur();
  }

  function ctrlBlur() {
    setTimeout(() => {
      if (document.activeElement !== ctrlInputElement) ctrlArmed = false;
    });
  }


  function jumpToBottom() {
    virtualStickToBottom = true;
    virtualScrollResetPending = true;
    renderVirtualWindow(Number.POSITIVE_INFINITY, true);
    void tick().then(() => {
      if (!terminalElement) {
        virtualScrollResetPending = false;
        return;
      }
      terminalElement.scrollTop = terminalElement.scrollHeight;
      virtualScrollResetPending = false;
      jumpVisible = false;
    });
  }

  function handleScroll() {
    if (virtualScrollResetPending) return;
    virtualStickToBottom = terminalElement.scrollHeight
      - terminalElement.scrollTop
      - terminalElement.clientHeight < 48;
    if (virtualStickToBottom) jumpVisible = false;
    scheduleVirtualWindow();
  }

  function paneSizeLeaseSupported(target: Agent): boolean {
    const connection = $connections.get(target.relay_id);
    return componentMounted
      && resizeLayout
      && !questionMode
      && connection?.status === 'connected'
      && connection.capabilities.includes('pane_size_lease');
  }

  function measuredPaneColumns(): number | null {
    if (!terminalElement || !cellMeasureElement) return null;
    const cellWidth = cellMeasureElement.getBoundingClientRect().width / CELL_MEASURE_TEXT.length;
    const style = getComputedStyle(terminalElement);
    const horizontalPadding = (Number.parseFloat(style.paddingLeft) || 0)
      + (Number.parseFloat(style.paddingRight) || 0);
    const usableWidth = terminalElement.clientWidth - horizontalPadding;
    if (!Number.isFinite(cellWidth) || cellWidth <= 0 || usableWidth <= 0) return null;
    return Math.min(
      MAX_PANE_SIZE_COLUMNS,
      Math.max(MIN_PANE_SIZE_COLUMNS, Math.floor(usableWidth / cellWidth)),
    );
  }

  function currentViewportWidth(): number {
    return window.visualViewport?.width || window.innerWidth;
  }

  async function applyCachedResizeFrame(cached: ResizedTerminalFrame) {
    const stick = pendingResizeStick ?? virtualStickToBottom;
    const previousTop = terminalElement?.scrollTop || 0;
    const previousAnchor = stick
      ? null
      : pendingResizeAnchor || currentVirtualAnchor(previousTop);
    pendingResizeStick = null;
    pendingResizeAnchor = null;
    virtualStickToBottom = stick;
    virtualScrollResetPending = Boolean(terminalElement);
    displayed = cached.display;
    renderedHtml = cached.html;
    renderedRows = cached.rows;
    lastContent = cached.frame.content;
    lastFormat = cached.frame.format;
    lastPreserveLayout = true;
    lastPreserveLineEnds = false;
    const nextTop = resetVirtualRows(
      stick ? Number.POSITIVE_INFINITY : previousTop,
      previousAnchor,
    );
    await tick();
    if (!terminalElement) {
      virtualScrollResetPending = false;
      return;
    }
    terminalElement.scrollLeft = 0;
    terminalElement.scrollTop = stick ? terminalElement.scrollHeight : nextTop;
    virtualScrollResetPending = false;
    jumpVisible = !stick;
    observeVirtualRows();
  }

  function rememberCurrentResizeFrame(
    next: TerminalFrame,
    display: string,
    html: string,
    rows: RenderedTerminalRow[],
  ) {
    if (!resizeSessionActive || lastLeasedColumns < 1) return;
    rememberResizedTerminalFrame(agent.pane_id, {
      frame: next,
      columns: lastLeasedColumns,
      display,
      html,
      rows,
      historyLines: $terminalHistoryLines,
      interfaceSize: $interfaceSize,
      viewportWidth: currentViewportWidth(),
    });
  }

  function validResizedTerminalFrame(value: TerminalFrame | undefined): ResizedTerminalFrame | null {
    const cached = resizedTerminalFrames.get(agent.pane_id);
    if (!value
      || value !== cached?.frame
      || cached.historyLines !== $terminalHistoryLines
      || cached.interfaceSize !== $interfaceSize
      || Math.abs(cached.viewportWidth - currentViewportWidth()) >= 1) return null;
    return cached;
  }

  function terminalFrameLineCount(value: TerminalFrame | undefined): number {
    if (!value?.content) return 0;
    let lines = 1;
    for (let index = 0; index < value.content.length; index += 1) {
      if (value.content.charCodeAt(index) === 10) lines += 1;
    }
    return lines;
  }

  function beginResizeSettling() {
    if (resizeReadTimer) clearTimeout(resizeReadTimer);
    resizeReadTimer = undefined;
    resizeFrameBaseline = frame;
    resizeExpectedLines = Math.min(terminalFrameLineCount(frame), $terminalHistoryLines);
    resizeSettleDeadline = Date.now() + PANE_SIZE_SETTLE_TIMEOUT_MS;
    resizeReadPending = true;
  }

  function scheduleSettledPaneRead(target: Agent, generation: number) {
    if (resizeReadTimer) clearTimeout(resizeReadTimer);
    resizeReadPending = true;
    resizeReadTimer = setTimeout(() => {
      resizeReadTimer = undefined;
      if (generation !== leaseGeneration
        || leaseTarget?.pane_id !== target.pane_id
        || !paneSizeLeaseSupported(target)) {
        resizeReadPending = false;
        return;
      }
      resizeFrameBaseline = frame;
      resizeReadPending = false;
      relayStore.readPane(target, true);
    }, PANE_SIZE_SETTLE_MS);
  }

  function clearResizeSettling() {
    if (resizeReadTimer) clearTimeout(resizeReadTimer);
    resizeReadTimer = undefined;
    resizeReadPending = false;
    resizeExpectedLines = 0;
    resizeSettleDeadline = 0;
  }

  function discardPaneSizeLease() {
    leaseGeneration += 1;
    leaseTarget = null;
    lastLeasedColumns = 0;
    resizeFrameBaseline = undefined;
    clearResizeSettling();
    queuedLease = null;
  }

  function releasePaneSizeLease(reportFailure: boolean) {
    const target = leaseTarget;
    discardPaneSizeLease();
    if (!target) return;
    void relayStore.releasePaneSize(target).catch((error) => {
      const connection = $connections.get(target.relay_id);
      if (reportFailure && componentMounted && connection?.status === 'connected') {
        paneSizeLeaseError = `Resize Session release failed: ${(error as Error).message}`;
      }
    });
  }

  function requestPaneSizeLease(force: boolean) {
    const target = agent;
    if (!paneSizeLeaseSupported(target)) return;
    const columns = measuredPaneColumns();
    if (columns === null) {
      if (terminalElement && cellMeasureElement) {
        paneSizeLeaseError = 'Resize Session could not measure the terminal cell width.';
      }
      return;
    }
    const sameTarget = leaseTarget?.pane_id === target.pane_id;
    if (!force && sameTarget && columns === lastLeasedColumns) return;
    if (queuedLease && queuedLease.columns === columns) {
      queuedLease = { columns, force: queuedLease.force || force };
    } else queuedLease = { columns, force };
    if (!leaseInFlight) void flushPaneSizeLease();
  }

  async function flushPaneSizeLease() {
    if (leaseInFlight) return;
    leaseInFlight = true;
    try {
      while (queuedLease) {
        const request = queuedLease;
        queuedLease = null;
        const target = agent;
        if (!paneSizeLeaseSupported(target)) continue;
        if (!request.force
          && leaseTarget?.pane_id === target.pane_id
          && request.columns === lastLeasedColumns) continue;
        if (leaseTarget && leaseTarget.pane_id !== target.pane_id) {
          releasePaneSizeLease(componentMounted);
        }
        const generation = leaseGeneration;
        leaseTarget = target;
        try {
          const resizing = request.columns !== lastLeasedColumns;
          if (resizing) beginResizeSettling();
          const appliedColumns = await relayStore.leasePaneSize(target, request.columns);
          if (generation !== leaseGeneration
            || leaseTarget?.pane_id !== target.pane_id
            || !paneSizeLeaseSupported(target)) continue;
          const changed = appliedColumns !== lastLeasedColumns;
          lastLeasedColumns = appliedColumns;
          paneSizeLeaseError = '';
          if (changed) scheduleSettledPaneRead(target, generation);
        } catch (error) {
          if (generation === leaseGeneration
            && leaseTarget?.pane_id === target.pane_id
            && paneSizeLeaseSupported(target)) {
            queuedLease = null;
            lastLeasedColumns = 0;
            clearResizeSettling();
            paneSizeLeaseError = `Resize Session failed: ${(error as Error).message}`;
          }
        }
      }
    } finally {
      leaseInFlight = false;
      if (queuedLease) void flushPaneSizeLease();
    }
  }


  async function filesSelected(files: FileList | File[]) {
    for (const file of [...files].filter((item) => item.type.startsWith('image/'))) {
      uploadStatus = `Uploading ${file.name || 'image'}…`;
      uploadError = false;
      try {
        const path = await relayStore.uploadImage(agent, file);
        const prefix = composer && !composer.endsWith('\n') ? '\n' : '';
        composer += `${prefix}Image: ${path}\n`;
        uploadStatus = `Image attached: ${path.split(/[\\/]/).pop() || 'image'}`;
      } catch (error) {
        uploadStatus = (error as Error).message;
        uploadError = true;
      }
    }
  }

  function paste(event: ClipboardEvent) {
    const files = [...(event.clipboardData?.items || [])]
      .filter((item) => item.kind === 'file' && item.type.startsWith('image/'))
      .map((item) => item.getAsFile())
      .filter((file): file is File => Boolean(file));
    if (!files.length) return;
    event.preventDefault();
    void filesSelected(files);
  }

  function openNext() {
    if (nextBlocked) replaceView({ view: 'terminal', paneId: nextBlocked.pane_id });
  }
</script>

<main
  class:has-actions={inputLocked || nextBlocked}
  class:question-only={questionMode}
  class="terminal-view"
  aria-label={`${questionMode ? 'Questions' : 'Terminal'} for ${agent.project || agent.name || agent.agent || 'agent'}`}
>
  {#if questionMode && interaction}
    <QuestionForm {agent} {interaction} responding={responding.has(agent.pane_id)} />
    <div class="term-keys question-term-keys" aria-label="Terminal fallback keys">
      <Button variant="secondary" size="sm" onclick={() => sendKeys(['Escape'], 'Cancelled prompt')}>Esc</Button>
      <Button variant="secondary" size="sm" onclick={() => sendKeys(['Tab'])}>Tab</Button>
      <span class="spacer"></span>
      <div class="arrow-menu">
        <Button variant="secondary" size="sm" aria-label="Arrow keys" aria-expanded={arrowsOpen} onclick={() => { arrowsOpen = !arrowsOpen; }}>
          <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <path d="M12 2v20M2 12h20"></path>
            <path d="m8 6 4-4 4 4M8 18l4 4 4-4M6 8l-4 4 4 4M18 8l4 4-4 4"></path>
          </svg>
        </Button>
        {#if arrowsOpen}
          <div class="arrow-popup">
            <span></span><button aria-label="Up" onclick={() => sendKeys(['Up'])}>↑</button><span></span>
            <button aria-label="Left" onclick={() => sendKeys(['Left'])}>←</button><span></span><button aria-label="Right" onclick={() => sendKeys(['Right'])}>→</button>
            <span></span><button aria-label="Down" onclick={() => sendKeys(['Down'])}>↓</button><span></span>
          </div>
        {/if}
      </div>
      <Button variant="secondary" size="sm" aria-label="Enter" onclick={() => sendKeys(['Enter'])}>Enter</Button>
    </div>
  {:else}
  <div
    class:resize-layout={resizeSessionActive}
    class:preserve-layout={preserveLayout}
    class="term-content"
    style={terminalContentStyle}
    bind:this={terminalElement}
    role="log"
    aria-label="Agent terminal output"
    onscroll={handleScroll}
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
  <textarea
    class="sr-only"
    aria-label="Full terminal transcript"
    readonly
    tabindex="-1"
    bind:this={transcriptElement}
    value={terminalPlainText}
  ></textarea>
  <div class="terminal-copy">
    <Button variant="secondary" size="sm" onclick={copyTerminalOutput}>Copy</Button>
  </div>
  {#if jumpVisible}
    <button class="jump-bottom" aria-label="Jump to latest output" onclick={jumpToBottom}>↓</button>
  {/if}

  <div class="terminal-bottom" onfocusin={focusComposer} onfocusout={blurComposer}>
    {#if slashMenuOpen}
      <section class="slash-command-popover" aria-label="Command suggestions">
        <header class="slash-command-header" aria-hidden="true">
          <strong>Commands</strong>
          {#if !slashCatalogLoading && !slashCatalogUnavailable}
            <span>{filteredSlashCommands.length} matching</span>
          {:else}
            <span>Type to filter</span>
          {/if}
        </header>
        {#if slashCatalogLoading}
          <p class="slash-command-status" role="status">Loading commands…</p>
        {:else if slashCatalogUnavailable}
          <p class="slash-command-status">Suggestions unavailable — you can still send this command.</p>
        {:else if !filteredSlashCommands.length}
          <p class="slash-command-status">No matching command — you can still send it.</p>
        {/if}
        <div
          id="slash-command-options"
          class="slash-command-menu"
          role="listbox"
          aria-label="Slash commands"
          aria-busy={slashCatalogLoading}
        >
          {#each filteredSlashCommands as entry, index (entry.command)}
            <button
              id={`slash-command-option-${index}`}
              type="button"
              role="option"
              tabindex="-1"
              class:active={index === effectiveSlashIndex}
              aria-selected={index === effectiveSlashIndex}
              onpointerdown={(event) => event.preventDefault()}
              onpointerenter={() => { activeSlashIndex = index; }}
              onclick={() => selectSlashCommand(entry)}
            >
              <span class="slash-command-name">
                <strong>{entry.command}</strong>
                {#if entry.argument_hint}<small>{entry.argument_hint}</small>{/if}
              </span>
              <span class="slash-command-description">{entry.description}</span>
              {#if entry.source !== 'builtin'}<em class="slash-command-source">{entry.source}</em>{/if}
            </button>
          {/each}
        </div>
        {#if !slashCatalogLoading && slashCatalog.truncated}
          <p class="slash-command-limit">More commands are available; keep typing to narrow the list.</p>
        {/if}
      </section>
    {/if}
    <div class="term-input">
      <Button variant="ghost" size="icon" disabled={inputLocked} aria-label="Attach image" onclick={() => fileInput.click()}>
        <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
          <rect x="3" y="4" width="18" height="16" rx="2"></rect>
          <circle cx="8.5" cy="9" r="1.5"></circle>
          <path d="m4 17 4.5-4.5 3.5 3.5 2.5-2.5L20 19"></path>
        </svg>
      </Button>
      <div class:awaiting-approval={approvalMode && !composerFocused} class:has-text={Boolean(composer)} class="composer-field">
        <textarea
          bind:this={composerElement}
          bind:value={composer}
          rows="1"
          disabled={inputLocked && !composerFocused}
          placeholder={approvalMode
            ? 'Approval pending — use buttons'
            : inspectionMode
              ? 'Needs inspection — use terminal keys'
              : 'Type a reply…'}
          role="combobox"
          aria-label="Prompt"
          aria-autocomplete="list"
          aria-haspopup="listbox"
          aria-expanded={slashMenuOpen}
          aria-controls={slashMenuOpen ? 'slash-command-options' : undefined}
          aria-activedescendant={slashMenuOpen && effectiveSlashIndex >= 0 ? `slash-command-option-${effectiveSlashIndex}` : undefined}
          autocomplete="off"
          autocorrect="on"
          autocapitalize="sentences"
          spellcheck="true"
          enterkeyhint="enter"
          oninput={composerInput}
          onkeydown={keydown}
          onpaste={paste}
        ></textarea>
        {#if composer}<button class="input-clear" aria-label="Clear prompt text" onclick={clearComposer}>×</button>{/if}
      </div>
      <Button size="icon" disabled={!composer.replace(/[\r\n]+$/g, '') || inputLocked} aria-label="Send prompt" onclick={sendPrompt}>➤</Button>
      <input bind:this={fileInput} type="file" accept="image/*" multiple hidden onchange={(event) => { void filesSelected(event.currentTarget.files || []); event.currentTarget.value = ''; }} />
    </div>
    {#if uploadStatus}<p class:error={uploadError} class="upload-status" role="status">{uploadStatus}</p>{/if}
    {#if paneSizeLeaseError}<p class="upload-status error" role="alert">{paneSizeLeaseError}</p>{/if}

    {#if approvalMode && !responding.has(agent.pane_id)}
      <div class="quick-actions" aria-label="Approval choices">
        {#each options as option, index (`${index}:${option}`)}
          <Button
            variant={approvalButtonTone(option, index, options.length) === 'deny' ? 'danger' : approvalButtonTone(option, index, options.length) === 'trust' ? 'trust' : 'default'}
            onclick={() => relayStore.respond(agent, index, options.length, option)}
          >{option}</Button>
        {/each}
        {#if nextBlocked}<Button variant="secondary" onclick={openNext}>Next blocked →</Button>{/if}
      </div>
    {:else if nextBlocked}
      <div class="quick-actions"><Button variant="secondary" onclick={openNext}>Next blocked →</Button></div>
    {/if}

    <div class="term-keys">
      <Button variant="secondary" size="sm" onclick={() => sendKeys(['Escape'], 'Cancelled prompt')}>Esc</Button>
      <Button variant="secondary" size="sm" onclick={() => sendKeys(['Tab'])}>Tab</Button>
      <Button variant="secondary" size="sm" onclick={() => sendKeys(['Shift+Tab'], 'Shift+Tab')}>Shift+Tab</Button>
      <div class="ctrl-menu">
        <input
          id="ctrl-key-input"
          class="ctrl-key-input"
          bind:this={ctrlInputElement}
          aria-label="Ctrl shortcut letter"
          autocomplete="off"
          autocapitalize="none"
          maxlength="1"
          spellcheck="false"
          oninput={ctrlInput}
          onkeydown={ctrlKeydown}
          onblur={ctrlBlur}
        />
        <Button
          variant="secondary"
          size="sm"
          aria-controls="ctrl-key-input"
          aria-pressed={ctrlArmed}
          title="Press Ctrl, then type a letter"
          onclick={toggleCtrl}
        >Ctrl</Button>
      </div>
      <span class="spacer"></span>
      <div class="arrow-menu">
        <Button
          variant="secondary"
          size="sm"
          aria-label="Arrow keys"
          aria-expanded={arrowsOpen}
          onclick={() => {
            ctrlArmed = false;
            ctrlInputElement.blur();
            arrowsOpen = !arrowsOpen;
          }}
        >
          <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <path d="M12 2v20M2 12h20"></path>
            <path d="m8 6 4-4 4 4M8 18l4 4 4-4M6 8l-4 4 4 4M18 8l4 4-4 4"></path>
          </svg>
        </Button>
        {#if arrowsOpen}
          <div class="arrow-popup">
            <span></span><button aria-label="Up" onclick={() => sendKeys(['Up'])}>↑</button><span></span>
            <button aria-label="Left" onclick={() => sendKeys(['Left'])}>←</button><span></span><button aria-label="Right" onclick={() => sendKeys(['Right'])}>→</button>
            <span></span><button aria-label="Down" onclick={() => sendKeys(['Down'])}>↓</button><span></span>
          </div>
        {/if}
      </div>
      <Button variant="secondary" size="sm" aria-label="Enter" onclick={() => sendKeys(['Enter'])}>Enter</Button>
    </div>
  </div>
  {/if}
</main>

<script lang="ts">
  import { onDestroy, onMount, tick, untrack } from 'svelte';
  import PromptComposer from '$components/PromptComposer.svelte';
  import TerminalKeys, { type QueuedKeyCommand } from '$components/TerminalKeys.svelte';
  import AgentInteraction from '$components/AgentInteraction.svelte';
  import TerminalInputMode from '$components/TerminalInputMode.svelte';
  import { promptDispatchRestriction } from '$lib/terminal-input';
  import type { ComposerSubmitAdapter } from '$lib/composer';
  import Button from '$components/ui/Button.svelte';
  import KeyboardIcon from '$components/ui/KeyboardIcon.svelte';
  import {
    MAX_PANE_SIZE_COLUMNS,
    MAX_PANE_SIZE_ROWS,
    MIN_PANE_SIZE_COLUMNS,
    MIN_PANE_SIZE_ROWS,
    paneLeaseRenewalAllowed,
  } from '$lib/config';
  import {
    agentNeedsInspection,
    agentNeedsResponse,
    attentionKind,
    questionInteraction,
    sortedAgents,
  } from '$lib/agents';
  import {
    findTerminalText,
    terminalMatchFragments,
    terminalRowForOffset,
    terminalRowOffsets,
    terminalSearchText,
  } from '$lib/terminal-find';
  import {
    armSpeechKeepalive,
    releaseSpeechKeepalive,
    speakViaRelay,
    speechEnabled,
    speechLanguage,
    speechLanguageLabel,
    speechState,
    stopSpeech,
  } from '$lib/speech';
  import { interfaceSize, terminalHeightLease, terminalResizeSession, theme } from '$lib/preferences';
  import { replaceView } from '$lib/router';
  import { targetRefForAgent } from '$lib/resource-id';
  import { securityState } from '$lib/security';
  import { terminalDraftState } from '$lib/terminal-draft-state.svelte';
  import { relayStore } from '$lib/store';
  import {
    latestCompletedResponse,
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
  let findInputElement = $state<HTMLInputElement>(null!);
  let transcriptElement = $state<HTMLTextAreaElement>(null!);
  let responseElement = $state<HTMLTextAreaElement>(null!);
  let agentResponsePreviewElement = $state<HTMLTextAreaElement>(null!);
  let copiedAgentResponseText = $state('');
  let historyTruncated = $state(false);
  // Pre-resize frame kept only to suppress display of transient frames while
  // the agent repaints at the new width.
  // Raw state: the effect compares frame identity, which a deep proxy breaks.
  let resizeFrameBaseline = $state.raw<TerminalFrame | undefined>(undefined);
  // The relay flags frames while a pane repaints at a new size. Suppressing
  // them keeps a half-drawn screen off the phone, but a shared session whose
  // desktop client keeps fighting the leased size can flag every frame, so the
  // wait is bounded: a stale screen is worse than a transient one.
  let resizeWaitExpired = $state(false);
  let resizeWaitTimer: ReturnType<typeof setTimeout> | null = null;
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
  let wideGridOffsetsPane = '';
  let lastFormat = '';
  let lastContent = '';
  let lastPreserveLayout = false;
  let lastPreserveLineEnds = false;
  let lastRenderColumnCap = 0;
  let findOpen = $state(false);
  let findQuery = $state('');
  let activeFindIndex = $state(-1);
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
  let copyingAgentResponse = $state(false);
  let paneSizeLeaseError = $state('');
  let requestedPaneId = '';
  let dismissedMenuSignature = $state('');
  const CELL_MEASURE_TEXT = '0000000000';
  const PANE_SIZE_LEASE_REFRESH_MS = 10_000;
  const PANE_REALTIME_RESYNC_MS = 15_000;
  // One relay poll of slack over its own three-second settling window.
  const PANE_RESIZE_WAIT_MAX_MS = 4_000;
  const RESPONSE_COPY_AGENT_IDS = new Set([
    'hermes', 'hermesagent',
    'claude', 'claudecode', 'codex', 'openaicodex', 'kimi', 'kimicode',
    'omp', 'ohmypi', 'pi', 'picodingagent', 'qoder', 'qodercli',
  ]);
  function responseCopyProfileSupported(agentName: unknown): boolean {
    const normalized = String(agentName || '').trim().toLocaleLowerCase().replace(/\s+/g, '').replace(/-/g, '');
    return RESPONSE_COPY_AGENT_IDS.has(normalized);
  }
  let componentMounted = false;
  let leaseGeneration = 0;
  let leaseInFlight = false;
  let leaseTarget: Agent | null = null;
  let lastLeasedColumns = $state(0);
  let lastLeasedRows = 0;
  let renderedResizeColumns = $state(0);
  let measuredCellWidth = $state(0);
  let queuedLease: { columns: number; rows: number; force: boolean } | null = null;
  // Set when the page goes hidden; renewals continue within the grace so a
  // desktop browser that reports occluded windows as hidden does not lapse
  // the lease on every app switch.
  let hiddenAt = 0;
  // A locked app is showing this pane to nobody who has verified, so it counts
  // as hidden for reads and watches. The connection now survives the lock, so
  // without this the terminal would keep streaming behind the unlock screen.
  const paneVisible = () => document.visibilityState === 'visible' && !$securityState.locked;
  // The lease needs its own gate: being locked is not the same as being
  // briefly hidden. The grace window below exists so an app switch does not
  // resize the computer's pane, but an unverified holder must not keep it
  // narrowed at all, so a lock skips the grace instead of entering it. The
  // relay's TTL hands the size back if the lock outlasts it.
  const paneLeaseAllowed = () => !$securityState.locked
    && paneLeaseRenewalAllowed(document.visibilityState === 'visible', hiddenAt, Date.now());
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
    lastLeasedColumns || renderedResizeColumns,
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
  let inputBlocked = $state(false);
  // Literal terminal input is the escape hatch for when the composer cannot send: the agent awaits a structured
  // response or needs inspection, or an earlier terminal send is unconfirmed and must be inspected first.
  const manualInputOffered = $derived(!readOnly && (Boolean(promptDispatchRestriction(agent)) || inputBlocked));
  const manualInputBusy = $derived(sendingPrompt || keySending || responding.has(agent.pane_id));
  const dispatchBlocked = $derived(responsePending || !terminalTextMode ? promptDispatchRestriction(agent) : '');
  const draftState = terminalDraftState(() => agent, () => frame);
  const noEchoActive = $derived(Boolean(frame?.paneId === agent.pane_id && frame?.noEcho));
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
  const terminalFindCorpus = $derived.by(() => ({
    text: terminalSearchText(renderedRows),
    offsets: terminalRowOffsets(renderedRows),
  }));
  const terminalFind = $derived(findTerminalText(terminalFindCorpus.text, findQuery.trim()));
  const agentResponseCopySupported = $derived.by(() => {
    const connection = $connections.get(agent.relay_id);
    return Boolean(
      connection?.capabilities.includes('agent_response_copy')
      && responseCopyProfileSupported(agent.agent),
    );
  });
  const relaySpeechLanguages = $derived($connections.get(agent.relay_id)?.speechLanguages ?? []);
  const terminalCopyText = $derived(latestCompletedResponse(frame?.content || ''));
  const terminalContentStyle = $derived.by(() => {
    // Every width is emitted in px of the measured probe cell, never in ch:
    // Safari's Core Text port resolves 1ch against different font metrics
    // than the rendered glyph advance, so an Nch cap wraps short of N cells.
    // Before the probe is measured no cap is emitted at all. The wrapping
    // layout is gated on the same predicate, so the missing cap can no longer
    // fall back to the container width and shred rows mid-word: the fixed
    // layout applies instead until a real cap exists.
    if (measuredCellWidth <= 0) return undefined;
    const capColumns = lastLeasedColumns || renderedResizeColumns;
    const styles = [`--terminal-cell-width: ${measuredCellWidth.toFixed(4)}px`];
    if (resizeLayoutActive) {
      styles.push(`--terminal-width: ${(capColumns * measuredCellWidth).toFixed(4)}px`);
    }
    if (virtualContentColumns > 0) {
      styles.push(`--terminal-content-width: ${(virtualContentColumns * measuredCellWidth).toFixed(4)}px`);
    }
    return styles.join(';');
  });

  $effect(() => {
    const count = terminalFind.matches.length;
    const query = findQuery.trim();
    untrack(() => {
      if (!query || !count) {
        activeFindIndex = -1;
        return;
      }
      if (activeFindIndex < 0 || activeFindIndex >= count) activeFindIndex = 0;
    });
  });

  $effect(() => {
    const highlightState = [
      findOpen,
      findQuery,
      activeFindIndex,
      virtualHtml,
      terminalFind.matches.length,
    ];
    void highlightState;
    void tick().then(applyTerminalFindHighlights);
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
      void applyFrame(next, lastPreserveLayout, lastPreserveLineEnds);
    });
  });

  $effect(() => {
    if (questionMode || terminalLayoutSuspended) return;
    const next = frame;
    const matchingFrame = next?.paneId === agent.pane_id ? next : undefined;
    const preserve = true;
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
      && !paneSizeLeaseError
      && !resizeWaitExpired
      && (lastLeasedColumns === 0
        || (Boolean(resizeFrameBaseline)
          && (next === resizeFrameBaseline || next?.resizeSettling === true)));
    const cachedFrame = waitingForResizedFrame
      && matchingFrame
      && matchingFrame.resizeSettling !== true
      && (!resizeFrameBaseline || matchingFrame === resizeFrameBaseline)
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
        untrack(() => { void applyFrame(cachedFrame, preserve, preserveLineEnds) });
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
    if (resizeSessionActive && lastLeasedColumns > 0) renderedResizeColumns = lastLeasedColumns;
    historyTruncated = Boolean(matchingFrame.truncated);
    untrack(() => { void applyFrame(matchingFrame, preserve, preserveLineEnds) });
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
    const locked = $securityState.locked;
    if (locked === paneLocked) return;
    paneLocked = locked;
    paneVisibilityChanged?.();
  });

  $effect(() => {
    const connection = $connections.get(agent.relay_id);
    const interfaceSizeValue = $interfaceSize;
    const paneId = agent.pane_id;
    void interfaceSizeValue;
    if (!$terminalResizeSession) {
      // Turned off: the store has already released every pane this phone
      // leased, immediately. Only the local lease state is left to drop.
      discardPaneSizeLease();
      paneSizeLeaseError = '';
      return;
    }
    if (!resizeSessionActive || readOnly) {
      releasePaneSizeLease(componentMounted);
      paneSizeLeaseError = '';
      return;
    }
    if (questionMode) {
      releasePaneSizeLease(componentMounted);
      paneSizeLeaseError = '';
      return;
    }
    if (connection?.status !== 'connected') {
      discardPaneSizeLease();
      paneSizeLeaseError = '';
      return;
    }
    if (leaseTarget && leaseTarget.pane_id !== paneId) releasePaneSizeLease(componentMounted);
    paneSizeLeaseError = '';
    void tick().then(() => requestPaneSizeLease(false));
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
      requestPaneSizeLease(false);
    });
    observer.observe(element);
    return () => observer.disconnect();
  });

  onMount(() => {
    componentMounted = true;
    relayStore.readPane(agent, true);
    const stopWakeLock = mountTerminalWakeLock();
    const measurePane = () => requestPaneSizeLease(false);
    const realtimeDeltaEnabled = () => Boolean(
      $connections.get(agent.relay_id)?.capabilities.includes('pane_realtime_delta'),
    );
    let lastRefreshAt = Date.now();
    const visibilityChanged = () => {
      if (!paneVisible()) {
        // Traffic stops while hidden, but lease renewals keep going for a
        // bounded grace: desktop Safari reports an occluded window as hidden,
        // so treating every app switch as departure lapsed the lease and
        // resized the shared pane twice per glance, stranding stale copies of
        // inline agents' status bars in the scrollback. Once the grace runs
        // out the renewals stop and the relay's TTL hands the size back — a
        // page hidden overnight cannot keep the pane narrow.
        hiddenAt = Date.now();
        relayStore.unwatchPane(agent);
        return;
      }
      hiddenAt = 0;
      lastRefreshAt = Date.now();
      relayStore.readPane(agent);
      relayStore.watchPane(agent);
      // Re-lease at once: if the lease lapsed while asleep, the pane is back
      // at the desktop width until this lands.
      requestPaneSizeLease(true);
    };
    paneVisibilityChanged = visibilityChanged;
    const findShortcut = (event: KeyboardEvent) => {
      if (questionMode
        || event.altKey
        || (!event.ctrlKey && !event.metaKey)
        || event.key.toLocaleLowerCase() !== 'f') return;
      event.preventDefault();
      openTerminalFind();
    };
    window.addEventListener('keydown', findShortcut);
    window.addEventListener('resize', measurePane);
    window.visualViewport?.addEventListener('resize', measurePane);
    document.addEventListener('visibilitychange', visibilityChanged);
    const refresh = setInterval(() => {
      if (!paneVisible()) return;
      const refreshInterval = realtimeDeltaEnabled() ? PANE_REALTIME_RESYNC_MS : 3_000;
      if (Date.now() - lastRefreshAt < refreshInterval) return;
      lastRefreshAt = Date.now();
      relayStore.readPane(agent);
    }, 3_000);
    if (paneVisible()) relayStore.watchPane(agent);
    const refreshPaneSizeLease = setInterval(
      () => {
        if (paneLeaseAllowed()) requestPaneSizeLease(true);
      },
      PANE_SIZE_LEASE_REFRESH_MS,
    );
    void tick().then(measurePane);
    return () => {
      componentMounted = false;
      window.removeEventListener('resize', measurePane);
      window.visualViewport?.removeEventListener('resize', measurePane);
      document.removeEventListener('visibilitychange', visibilityChanged);
      paneVisibilityChanged = null;
      window.removeEventListener('keydown', findShortcut);
      clearInterval(refresh);
      clearInterval(refreshPaneSizeLease);
      relayStore.unwatchPane(agent);
      releasePaneSizeLease(false);
      virtualRowObserver?.disconnect();
      if (virtualWindowFrame) cancelAnimationFrame(virtualWindowFrame);
      stopWakeLock();
    };
  });

  async function applyFrame(
    next: TerminalFrame,
    preserve = true,
    preserveLineEnds = preserve && !resizeSessionActive && !readOnly,
  ) {
    if (terminalLayoutSuspended) return;
    const renderColumnCap = resizeLayoutActive
      ? lastLeasedColumns
      : (resizeLayoutPending ? measuredPaneColumns() || 0 : 0);
    const layoutChanged = preserve !== lastPreserveLayout
      || preserveLineEnds !== lastPreserveLineEnds
      || renderColumnCap !== lastRenderColumnCap;
    if (next.content === lastContent && next.format === lastFormat && !layoutChanged) return;
    const rendered = renderTerminalContent(
      next.content,
      next.format,
      preserve,
      preserveLineEnds,
      // Cap box-drawing rows at the width in force. A fixed-grid row is a run
      // of fixed-width cells with no wrap opportunity between them, so past
      // the cap it must render as plain text that can wrap instead. Zero means
      // "no cap", which only a relay that cannot lease is entitled to.
      renderColumnCap,
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
    lastPreserveLayout = preserve;
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
      lastPreserveLayout ? 'preserve' : 'readable',
      resizeLayoutActive ? 'resize' : 'fixed',
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
      // One line per row only where the row really stays on one line: a leased
      // pane's fixed-grid rows. Everything else wraps at the width in force,
      // and estimating those at one line parks the scroll past the content.
      // Rows wrap in the two regimes that have a width to wrap at: engaged
      // (the leased width, fixed grids excepted) and pending (the container).
      // A relay that cannot lease keeps every row on one line.
      const wraps = (!lastPreserveLayout
        || (resizeLayoutPending && !row.wideGrid)
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

    if (!lastPreserveLayout || resizeLayoutPending) virtualContentColumns = 0;
    else {
      virtualContentColumns = terminalScreenColumns(
        renderedRows,
        resizeLayoutActive,
        lastLeasedColumns || renderedResizeColumns,
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
    if (wideGridOffsetsPane !== agent.pane_id) {
      wideGridOffsets.clear();
      wideGridOffsetsPane = agent.pane_id;
    }
    if (wideGridOffsets.get(block) === row.scrollLeft) return;
    wideGridOffsets.set(block, row.scrollLeft);
    for (const sibling of terminalElement.querySelectorAll<HTMLElement>(`[data-terminal-wide-block="${block}"]`)) {
      if (sibling !== row && sibling.scrollLeft !== row.scrollLeft) sibling.scrollLeft = row.scrollLeft;
    }
  }

  function restoreWideGridScroll() {
    if (!terminalElement || wideGridOffsetsPane !== agent.pane_id) return;
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

  function clearTerminalFindHighlights() {
    if (!terminalElement) return;
    const parents = new Set<Node>();
    for (const mark of terminalElement.querySelectorAll<HTMLElement>('mark[data-terminal-find]')) {
      if (mark.parentNode) parents.add(mark.parentNode);
      mark.replaceWith(document.createTextNode(mark.textContent || ''));
    }
    for (const parent of parents) parent.normalize();
  }

  function applyTerminalFindHighlights() {
    clearTerminalFindHighlights();
    if (!terminalElement || !findOpen || !findQuery.trim() || !terminalFind.matches.length) return;
    const byRow = new Map<number, Array<{ start: number; end: number; active: boolean }>>();
    terminalFind.matches.forEach((match, matchIndex) => {
      for (const fragment of terminalMatchFragments(renderedRows, terminalFindCorpus.offsets, match)) {
        if (fragment.row < virtualStart || fragment.row >= virtualEnd) continue;
        const fragments = byRow.get(fragment.row) || [];
        fragments.push({ start: fragment.start, end: fragment.end, active: matchIndex === activeFindIndex });
        byRow.set(fragment.row, fragments);
      }
    });
    for (const [row, fragments] of byRow) {
      const element = terminalElement.querySelector<HTMLElement>(`[data-terminal-row="${row}"]`);
      if (element) highlightTerminalRow(element, fragments);
    }
  }

  function highlightTerminalRow(
    element: HTMLElement,
    fragments: Array<{ start: number; end: number; active: boolean }>,
  ) {
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    const nodes: Array<{ node: Text; start: number; end: number }> = [];
    let offset = 0;
    let node = walker.nextNode();
    while (node) {
      const text = node as Text;
      nodes.push({ node: text, start: offset, end: offset + text.data.length });
      offset += text.data.length;
      node = walker.nextNode();
    }
    for (const entry of nodes) {
      const intersections = fragments
        .map((fragment) => ({
          start: Math.max(fragment.start, entry.start) - entry.start,
          end: Math.min(fragment.end, entry.end) - entry.start,
          active: fragment.active,
        }))
        .filter((fragment) => fragment.end > fragment.start)
        .sort((left, right) => right.start - left.start);
      for (const fragment of intersections) {
        entry.node.splitText(fragment.end);
        const selected = entry.node.splitText(fragment.start);
        const mark = document.createElement('mark');
        mark.dataset.terminalFind = '';
        mark.className = `terminal-find-match${fragment.active ? ' active' : ''}`;
        selected.replaceWith(mark);
        mark.append(selected);
      }
    }
  }

  function openTerminalFind() {
    findOpen = true;
    void tick().then(() => {
      findInputElement?.focus();
      findInputElement?.select();
    });
  }

  // The header owns the Find control, so the terminal exposes opening it rather
  // than lifting findOpen out: kept here, the bar still closes with the pane.
  export function openFind() {
    openTerminalFind();
  }

  function closeTerminalFind() {
    findOpen = false;
  }

  function findInputChanged() {
    activeFindIndex = -1;
    void tick().then(() => {
      if (terminalFind.matches.length) revealFindMatch(0);
    });
  }

  function findKeydown(event: KeyboardEvent) {
    if (event.key === 'Escape') {
      event.preventDefault();
      closeTerminalFind();
      return;
    }
    if (event.key !== 'Enter') return;
    event.preventDefault();
    revealFindMatch(activeFindIndex + (event.shiftKey ? -1 : 1));
  }

  function revealFindMatch(index: number) {
    const count = terminalFind.matches.length;
    if (!terminalElement || !count) return;
    const normalized = ((index % count) + count) % count;
    activeFindIndex = normalized;
    const match = terminalFind.matches[normalized];
    const row = terminalRowForOffset(renderedRows, terminalFindCorpus.offsets, match.start);
    if (row < 0) return;
    const rowTop = terminalScreenOffset() + virtualIndex.offset(row);
    const nextTop = Math.max(0, rowTop - (terminalElement.clientHeight - virtualIndex.size(row)) / 2);
    virtualStickToBottom = false;
    virtualScrollResetPending = true;
    renderVirtualWindow(nextTop, true);
    void tick().then(() => {
      if (!terminalElement) return;
      terminalElement.scrollTop = nextTop;
      rememberVirtualScrollGeometry(terminalElement);
      virtualStickToBottom = terminalElement.scrollHeight
        - terminalElement.scrollTop
        - terminalElement.clientHeight < 48;
      virtualScrollResetPending = false;
      applyTerminalFindHighlights();
    });
  }

  let fetchingSpeechText = $state(false);

  interface AgentResponseSource {
    text: string;
    /** The agent's own text (relay copy or its transcript), not a terminal parse. */
    exact: boolean;
    failure: string;
  }

  /**
   * The agent's latest complete response, from the most exact source this
   * device may use. A controller runs the relay's copy transaction, which types
   * into the agent's terminal; a reader may not, so it reads the agent's own
   * transcript instead. The terminal parse is the last resort for both.
   * `haltOnCopyFailure` returns the relay's failure without a fallback.
   */
  async function latestAgentResponse(haltOnCopyFailure = false): Promise<AgentResponseSource> {
    let failure = '';
    if (!readOnly && agentResponseCopySupported) {
      try {
        const result = await relayStore.sendToAgent(agent, { type: 'copy_agent_response' }, 15_000);
        const text = String(result.data?.text || '');
        if (text.trim()) return { text, exact: true, failure: '' };
      } catch (error) {
        failure = error instanceof Error && error.message ? error.message : 'Could not copy the agent response.';
        if (haltOnCopyFailure) return { text: '', exact: false, failure };
      }
    }
    if (agent.conversation_history_available) {
      try {
        const page = await relayStore.getConversationHistory(agent, { limit: 8 });
        const latest = page.entries.findLast((entry) => entry.role === 'assistant' && entry.text.trim());
        if (latest) return { text: latest.text, exact: true, failure: '' };
      } catch (error) {
        failure ||= error instanceof Error && error.message ? error.message : 'Could not read the conversation.';
      }
    }
    return { text: terminalCopyText, exact: false, failure };
  }

  async function speakTerminalResponse() {
    if ($speechState === 'speaking') {
      stopSpeech();
      return;
    }
    if (fetchingSpeechText) return;
    const toast = (message: string) => relayStore.showToast(message, true);
    // Checked before anything plays: unlocking audio for a language the relay
    // cannot speak leaves the phone with a silent stream and no explanation.
    if (!relaySpeechLanguages.includes($speechLanguage)) {
      toast(`This relay has no ${speechLanguageLabel($speechLanguage)} voice; install a Piper voice for it on that computer.`);
      return;
    }
    // Armed before the relay round trip: the tap's activation window does not
    // survive the await, and audio started after it is autoplay-blocked.
    armSpeechKeepalive(toast);
    fetchingSpeechText = true;
    try {
      const { text, failure } = await latestAgentResponse();
      const spoke = text.trim() && speakViaRelay(
        text,
        (chunk, language) => relayStore.speakToAgent(agent, chunk, language),
        toast,
      );
      if (!spoke) {
        releaseSpeechKeepalive();
        if (!text.trim()) toast(failure || 'No completed agent response is available to read aloud.');
      }
    } finally {
      fetchingSpeechText = false;
    }
  }

  async function copyTerminalOutput() {
    copiedAgentResponseText = '';
    let text = '';
    let copiedAgentResponse = false;
    copyingAgentResponse = true;
    try {
      const response = await latestAgentResponse(true);
      if (response.failure && !response.text.trim()) {
        relayStore.showToast(response.failure, true);
        return;
      }
      if (response.exact) {
        text = response.text;
        copiedAgentResponse = true;
        copiedAgentResponseText = response.text;
      }
    } finally {
      copyingAgentResponse = false;
    }
    if (!text) text = terminalCopyText || terminalPlainText;
    if (!text.trim()) {
      relayStore.showToast('No terminal output is available to copy.', true);
      return;
    }
    const hasCompletedResponse = copiedAgentResponse || Boolean(terminalCopyText.trim());
    const target = hasCompletedResponse ? responseElement : transcriptElement;
    const copiedMessage = copiedAgentResponse
      ? 'Agent response copied.'
      : hasCompletedResponse
        ? 'Final response copied.'
        : 'Copied the visible terminal output.';
    const selectedMessage = copiedAgentResponse
      ? 'Output selected. Use your browser Copy command.'
      : hasCompletedResponse
        ? 'Final response selected. Use your browser Copy command.'
        : 'Output selected. Use your browser Copy command.';
    if (!navigator.clipboard?.writeText) {
      target.value = text;
      target.focus({ preventScroll: true });
      target.select();
      relayStore.showToast(selectedMessage);
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      relayStore.showToast(copiedMessage);
    } catch {
      target.value = text;
      target.focus({ preventScroll: true });
      target.select();
      relayStore.showToast(selectedMessage);
    }
  }

  async function copyDisplayedAgentResponse() {
    const text = copiedAgentResponseText;
    if (!text.trim()) return;
    if (!navigator.clipboard?.writeText) {
      agentResponsePreviewElement.focus({ preventScroll: true });
      agentResponsePreviewElement.select();
      relayStore.showToast('Output selected. Use your browser Copy command.');
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      relayStore.showToast('Agent response copied.');
    } catch {
      agentResponsePreviewElement.focus({ preventScroll: true });
      agentResponsePreviewElement.select();
      relayStore.showToast('Output selected. Use your browser Copy command.');
    }
  }

  function dismissCopiedAgentResponse() {
    copiedAgentResponseText = '';
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
    return resizeSessionActive
      && componentMounted
      && !readOnly
      && !questionMode
      && connection?.status === 'connected'
      && connection.capabilities.includes('pane_size_opt_in');
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
    void tick().then(() => requestPaneSizeLease(false));
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

  function beginResizeSettling() {
    resizeFrameBaseline = frame;
    resizeWaitExpired = false;
    if (resizeWaitTimer !== null) clearTimeout(resizeWaitTimer);
    resizeWaitTimer = setTimeout(() => {
      resizeWaitTimer = null;
      resizeWaitExpired = true;
    }, PANE_RESIZE_WAIT_MAX_MS);
  }

  function clearResizeSettling() {
    resizeFrameBaseline = undefined;
    resizeWaitExpired = false;
    if (resizeWaitTimer === null) return;
    clearTimeout(resizeWaitTimer);
    resizeWaitTimer = null;
  }

  function discardPaneSizeLease() {
    leaseGeneration += 1;
    leaseTarget = null;
    lastLeasedColumns = 0;
    lastLeasedRows = 0;
    renderedResizeColumns = 0;
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
    // A hidden page renews only within the grace window: after it, the
    // relay's lease TTL returns the desktop size, and the refocus handler
    // re-leases the moment the page is visible again.
    if (!paneLeaseAllowed()) return;
    if (!paneSizeLeaseSupported(target)) return;
    const columns = measuredPaneColumns();
    if (columns === null) {
      if (terminalElement && cellMeasureElement) {
        paneSizeLeaseError = 'Resize Session could not measure the terminal cell width.';
      }
      return;
    }
    let rows = paneSizeRowLeaseSupported(target) ? measuredPaneRows() : 0;
    // The on-screen keyboard shrinks the terminal while the user types, and
    // leasing that transient height would SIGWINCH the agent twice per
    // keyboard toggle. Every full-height redraw can strand a stale copy of a
    // bottom-anchored status bar in the scrollback, so while a text input
    // owns focus the lease keeps its resting height and may only grow; the
    // resize listeners re-measure the moment the keyboard closes.
    const active = document.activeElement;
    const typing = active instanceof HTMLTextAreaElement || active instanceof HTMLInputElement;
    if (typing && rows && lastLeasedRows && rows < lastLeasedRows) rows = lastLeasedRows;
    const sameTarget = leaseTarget?.pane_id === target.pane_id;
    if (!force && sameTarget && columns === lastLeasedColumns && rows === lastLeasedRows) return;
    if (queuedLease && queuedLease.columns === columns && queuedLease.rows === rows) {
      queuedLease = { columns, rows, force: queuedLease.force || force };
    } else queuedLease = { columns, rows, force };
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
          && request.columns === lastLeasedColumns
          && request.rows === lastLeasedRows) continue;
        if (leaseTarget && leaseTarget.pane_id !== target.pane_id) {
          releasePaneSizeLease(componentMounted);
        }
        const generation = leaseGeneration;
        leaseTarget = target;
        try {
          if (request.columns !== lastLeasedColumns
            || request.rows !== lastLeasedRows) beginResizeSettling();
          const applied = await relayStore.leasePaneSize(target, request.columns, request.rows);
          if (generation !== leaseGeneration
            || leaseTarget?.pane_id !== target.pane_id
            || !paneSizeLeaseSupported(target)) continue;
          const changed = applied.columns !== lastLeasedColumns
            || applied.rows !== lastLeasedRows;
          lastLeasedColumns = applied.columns;
          lastLeasedRows = applied.rows;
          paneSizeLeaseError = '';
          if (changed) {
            // The pane repaints at the new size: read again so the live
            // screen is the resized one. History stays with the relay journal.
            relayStore.readPane(target, true);
          }
        } catch (error) {
          if (generation === leaseGeneration
            && leaseTarget?.pane_id === target.pane_id
            && paneSizeLeaseSupported(target)) {
            queuedLease = null;
            lastLeasedColumns = 0;
            lastLeasedRows = 0;
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
{#snippet copyIcon()}
  <svg class="action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <rect x="9" y="9" width="11" height="12" rx="2"></rect>
    <path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"></path>
  </svg>
{/snippet}
{#snippet findPreviousIcon()}
  <svg class="find-action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="m6 15 6-6 6 6"></path>
  </svg>
{/snippet}

{#snippet findNextIcon()}
  <svg class="find-action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="m6 9 6 6 6-6"></path>
  </svg>
{/snippet}

{#snippet findCloseIcon()}
  <svg class="find-action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="m6 6 12 12M18 6 6 18"></path>
  </svg>
{/snippet}


<main
  class:has-actions={inputLocked || nextBlocked}
  class:question-only={questionMode && !manualInput}
  class:find-open={findOpen}
  class:reader={readOnly}
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
  {#if findOpen}
    <section class="terminal-find" aria-label="Find in terminal">
      <input
        bind:this={findInputElement}
        bind:value={findQuery}
        type="search"
        aria-label="Find in terminal output"
        placeholder="Find in terminal"
        autocomplete="off"
        autocapitalize="none"
        spellcheck="false"
        oninput={findInputChanged}
        onkeydown={findKeydown}
      />
      {#if findQuery.trim()}
        <span class="terminal-find-count" role="status" aria-live="polite">
          {#if !terminalFind.matches.length}
            No matches
          {:else}
            {activeFindIndex + 1} of {terminalFind.matches.length}{terminalFind.truncated ? '+' : ''}
          {/if}
        </span>
      {/if}
      <Button variant="secondary" size="sm" aria-label="Previous match" disabled={!terminalFind.matches.length} onclick={() => revealFindMatch(activeFindIndex - 1)}>
        {@render findPreviousIcon()}
      </Button>
      <Button variant="secondary" size="sm" aria-label="Next match" disabled={!terminalFind.matches.length} onclick={() => revealFindMatch(activeFindIndex + 1)}>
        {@render findNextIcon()}
      </Button>
      <Button variant="ghost" size="sm" aria-label="Close find" onclick={closeTerminalFind}>
        {@render findCloseIcon()}
      </Button>
    </section>
  {/if}
  <div class="term-wrap">
  <div
    class:resize-layout={resizeLayoutActive} class:resize-pending={resizeLayoutPending}
    class="term-content preserve-layout"
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
  <textarea
    class="sr-only"
    aria-label="Full terminal transcript"
    readonly
    tabindex="-1"
    bind:this={transcriptElement}
    value={terminalPlainText}
  ></textarea>
  <textarea
    class="sr-only"
    aria-label="Latest final response"
    readonly
    tabindex="-1"
    bind:this={responseElement}
    value={terminalCopyText}
  ></textarea>
  {#if copiedAgentResponseText}
    <section class="agent-response-preview" aria-label="Copied agent response">
      <div class="agent-response-preview-header">
        <strong>Markdown response</strong>
        <div class="agent-response-preview-actions">
          <Button variant="secondary" size="sm" onclick={copyDisplayedAgentResponse}>Copy markdown</Button>
          <Button variant="ghost" size="sm" onclick={dismissCopiedAgentResponse}>Dismiss</Button>
        </div>
      </div>
      <textarea
        aria-label="Copied agent response markdown"
        readonly
        bind:this={agentResponsePreviewElement}
        value={copiedAgentResponseText}
      ></textarea>
    </section>
  {/if}
  <div class="terminal-copy">
    <Button
      variant="secondary"
      size="sm"
      aria-label={copyingAgentResponse ? 'Copying…' : 'Copy'}
      aria-busy={copyingAgentResponse}
      title={copyingAgentResponse ? 'Copying…' : 'Copy output'}
      disabled={copyingAgentResponse || responding.has(agent.pane_id)}
      onclick={copyTerminalOutput}
    >{@render copyIcon()}</Button>
    {#if $speechEnabled}
      <Button
        variant="secondary"
        size="sm"
        aria-label={$speechState === 'speaking' ? 'Stop reading response' : 'Read latest response aloud'}
        title={$speechState === 'speaking' ? 'Stop reading' : `Read latest response in ${speechLanguageLabel($speechLanguage)}`}
        aria-busy={fetchingSpeechText}
        disabled={fetchingSpeechText}
        onclick={() => { void speakTerminalResponse(); }}
      >{$speechState === 'speaking' ? 'Stop' : 'Speak'}</Button>
    {/if}
  </div>

  </div>
  <div class="terminal-bottom">
    <TerminalInputMode bind:this={terminalInputMode} {agent} {frame} {readOnly} bind:manual={manualInput} bind:blocked={inputBlocked}
      bind:loading={manualInputLoading} busy={manualInputBusy} returnFocus={() => manualInputTriggerHost?.querySelector('button')?.focus()} />
    <div hidden={manualInput}>
    {#if !questionMode && !manualInput}<TerminalKeys queue={keyQueue} bind:this={terminalKeys} {agent} {readOnly} blocked={sendingFilter || inputBlocked} bind:sending={keySending} extra={manualInputTrigger} />{/if}
    {#key JSON.stringify(targetRefForAgent(agent))}
      <PromptComposer {agent} {frame} {readOnly} dispatchBlocked={manualInput || inputBlocked ? 'Check terminal input delivery before sending.' : draftState.secretReady ? '' : dispatchBlocked}
        blockExplained={manualInput || inputBlocked} attachmentsBlocked={inputLocked}
        {noEchoActive} draftPersistenceAllowed={draftState.allowed} noEchoPrompt={frame?.noEchoPrompt || 'Password:'}
        {submitAdapter} bind:sending={sendingPrompt}
        placeholder={terminalTextMode ? 'Type filter text…' : 'Type a reply…'}
        submitLabel={terminalTextMode ? 'Send filter text' : 'Send prompt'} />
    {/key}
    {#if paneSizeLeaseError}<p class="upload-status error" role="alert">{paneSizeLeaseError}</p>{/if}
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

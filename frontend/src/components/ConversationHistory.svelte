<script lang="ts">
  import { onDestroy, onMount, tick, untrack } from 'svelte';
  import PromptComposer from '$components/PromptComposer.svelte';
  import AgentInteraction from '$components/AgentInteraction.svelte';
  import { terminalInputDelivery } from '$lib/terminal-input-delivery.svelte';
  import { piPromptDelivery } from '$lib/pi-prompt.svelte';
  import { promptDispatchRestriction } from '$lib/terminal-input';
  import { targetRefForAgent } from '$lib/resource-id';
  import ConversationMessage from '$components/ConversationMessage.svelte';
  import OmoPlan from '$components/OmoPlan.svelte';
  import Button from '$components/ui/Button.svelte';
  import { displayName } from '$lib/agents';
  import { conversationEntries } from '$lib/conversation';
  import {
    ConversationHistoryController,
    type ConversationHistoryControllerState,
    HISTORY_MAX_PREPARATION_POLLS,
    HISTORY_MAX_CONTENT_BYTES,
    HISTORY_WIRE_PAGE_SIZE,
  } from '$lib/conversation-history';
  import {
    getConversationPreview,
    putConversationPreview,
  } from '$lib/conversation-cache';
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
  import { fencedCodeText } from '$lib/markdown';
  import { ConversationLiveReader, LIVE_MAX_BYTES, type LiveReaderState } from '$lib/conversation-live';
  import { ConversationMessageStateStore } from '$lib/conversation-message-state';
  import { securityState } from '$lib/security';
  import { terminalDraftState } from '$lib/terminal-draft-state.svelte';
  import { relayStore } from '$lib/store';
  import { stripAnsi } from '$lib/terminal';
  import { terminalTextInputMode } from '$lib/terminal-menu';
  import type { Agent, ConversationCapabilities, ConversationEntry, ConversationPage, OmoTodoState, TerminalFrame } from '$lib/types';

  let {
    agent,
    readOnly = false,
    frame,
    responding = new Set<string>(),
    onInitialPage,
  }: { agent: Agent; readOnly?: boolean; frame?: TerminalFrame; responding?: Set<string>; onInitialPage?: (page: ConversationPage) => void } = $props();

  const connections = relayStore.connections;
  let paneVisible = $state(true);
  const draftState = terminalDraftState(() => agent, () => frame);
  const paneConnectionStatus = $derived($connections.get(agent.relay_id)?.status);
  const paneStateIdentity = $derived(JSON.stringify(targetRefForAgent(agent)));
  $effect(() => {
    const identity = paneStateIdentity;
    const status = paneConnectionStatus;
    const target = untrack(() => ({ ...agent }));
    if (!controllerReady || !paneVisible || $securityState.locked || status !== 'connected') return;
    void identity;
    const refreshState = () => relayStore.readPane(target, false, 30);
    relayStore.readPane(target, true, 30);
    const refresh = setInterval(refreshState, 2_000);
    return () => clearInterval(refresh);
  });

  const messageStates = new ConversationMessageStateStore();
  let entries = $state<ConversationEntry[]>([]);
  let liveEntries = $state<ConversationEntry[]>([]);
  let liveState = $state<LiveReaderState>('unavailable');
  let liveCapabilities = $state<ConversationCapabilities>();
  let liveReader: ConversationLiveReader | null = null;
  let headPage: ConversationPage | null = null;
  let renderAnchor = $state<string | null>(null);
  const mountedEntryLimit = HISTORY_WIRE_PAGE_SIZE;
  let available = $state(true);
  let reason = $state('');
  let hasMore = $state(false);
  let nextCursor = $state('');
  let browseState = $state<ConversationPage['state']>('ready');
  let browseProgress = $state<ConversationPage['progress']>();
  let sourceChangedNotice = $state('');
  let diagnostics = $state<ConversationPage['diagnostics']>();
  let omoPlan = $state<OmoTodoState | null>(null);
  let loading = $state(true);
  let loadingOlder = $state(false);
  let error = $state('');
  let errorCode = $state('');
  let errorRetryable = $state(false);
  let query = $state('');
  let mode = $state<'conversation' | 'activity'>('conversation');
  let listElement = $state<HTMLElement>(null!);
  let streamElement = $state<HTMLElement>(null!);
  /**
   * Whether the view follows the end of the transcript. It starts pinned so
   * opening a session lands on the newest turn, and only the reader scrolling
   * away from the bottom releases it.
   */
  let pinnedToBottom = $state(true);
  let followPendingUntil = 0;
  let windowAdvancePending = false;
  let upwardDemand = false;
  let pinPendingUntil = 0;
  let pinTargetTop = 0;
  let pinTargetHeight = 0;
  let lastScrollTop = 0;
  let lastScrollHeight = 0;
  const bottomThreshold = 48;
  let pinPendingTimer: ReturnType<typeof setTimeout> | undefined;
  let mounted = false;
  let controllerReady = $state(false);
  let historyController: ConversationHistoryController | null = null;
  let previewVisible = $state(false);
  let previewHistorical = $state(false);
  let authoritative = $state(false);
  let contextSearching = $state(false);
  let pendingPrefix = $state<ConversationEntry[]>([]);
  let beginningReached = $state(false);
  let pausedReason = $state('');
  let latestGapOutstanding = $state(false);
  let preparationPolls = $state(0);
  let requestPhase = $state<'idle' | 'initial' | 'refresh' | 'older' | 'preparing'>('initial');
  let refreshTimer: ReturnType<typeof setInterval> | undefined;
  let topSentinel = $state<HTMLElement>(null!);
  const refreshIntervalMs = 5_000;
  const maxPreparationPolls = HISTORY_MAX_PREPARATION_POLLS;

  const agentName = $derived(displayName(agent));
  const modeEntries = $derived.by(() => {
    if (mode !== 'conversation') return entries;
    const compact = conversationEntries(entries);
    if (!pendingPrefix.length || !entries.some(entry => entry.role === 'user')) return compact;
    const pendingIds = new Set(pendingPrefix.map((entry) => entry.id));
    return compact.filter((entry) => entry.kind === 'notice' || !pendingIds.has(entry.id));
  });
  const draftPersistenceAllowed = $derived(draftState.allowed);
  const screenPickerActive = $derived(frame?.paneId === agent.pane_id && terminalTextInputMode(stripAnsi(frame.content)) === 'filter');
  const delivery = $derived(terminalInputDelivery(agent));
  const piDelivery = $derived(piPromptDelivery(agent));
  const inputBlocked = $derived(delivery.pending || delivery.uncertain || piDelivery.pending || piDelivery.uncertain);
  $effect.pre(() => {
    const state = delivery;
    const piState = piDelivery;
    state.retain();
    piState.retain();
    return () => { state.release(); piState.release(); };
  });
  const dispatchBlocked = $derived(promptDispatchRestriction(agent, screenPickerActive));
  const noEchoActive = $derived(Boolean(frame?.paneId === agent.pane_id && frame?.noEcho));
  const inputPlaceholder = $derived(readOnly ? 'Reader access is read only' : 'Type a reply…');
  function matchesSearch(entry: ConversationEntry, needle: string): boolean {
    return `${entry.text} ${(entry.tools || []).map(tool => `${tool.name} ${tool.input || ''} ${tool.output || ''}`).join(' ')}`.toLocaleLowerCase().includes(needle);
  }
  const matchingRecordedEntries = $derived.by(() => {
    const needle = query.trim().toLocaleLowerCase();
    if (!needle) return modeEntries;
    return modeEntries.filter(entry => matchesSearch(entry, needle));
  });
  const matchingLiveEntries = $derived.by(() => {
    if (mode !== 'activity') return [];
    const needle = query.trim().toLocaleLowerCase();
    return needle ? liveEntries.filter(entry => matchesSearch(entry, needle)) : liveEntries;
  });
  const filteredEntries = $derived([...matchingRecordedEntries, ...matchingLiveEntries]);
  const renderStart = $derived.by(() => {
    if (filteredEntries.length <= mountedEntryLimit) return 0;
    const anchor = !pinnedToBottom && renderAnchor ? filteredEntries.findIndex(entry => entry.id === renderAnchor) : -1;
    return anchor >= 0 ? anchor : Math.max(0, filteredEntries.length - mountedEntryLimit);
  });
  const visibleEntries = $derived(filteredEntries.slice(renderStart, renderStart + mountedEntryLimit));
  const branchUnverified = $derived(liveCapabilities?.source === 'recorded_history' && liveCapabilities.active_branch !== true);
  const liveStatusText = $derived(liveState === 'disconnected' ? 'Disconnected — loaded history remains readable.'
    : liveState === 'paused' ? 'Updates paused.'
    : liveState === 'synchronizing' ? 'Synchronizing updates…'
    : liveState === 'incomplete' ? 'Live output is incomplete — use Terminal for current activity.'
    : liveState === 'unavailable' ? 'Live events unavailable — recorded history only.'
    : '');
  const historyBusy = $derived(requestPhase !== 'idle');
  const historyStatusText = $derived.by(() => {
    if (pausedReason) return pausedReason;
    if (latestGapOutstanding) return 'Checking for new messages…';
    if (requestPhase === 'preparing') return 'Loading conversation context…';
    if (contextSearching) return 'Loading conversation context…';
    if (previewVisible && !authoritative) return 'Checking for new messages…';
    if (requestPhase === 'older') return 'Loading earlier messages…';
    if (requestPhase === 'initial' && !entries.length) return 'Loading conversation…';
    if (beginningReached && entries.length) {
      return diagnostics?.source_truncated || diagnostics?.oversized_records || diagnostics?.corrupt_records
        ? 'Beginning of available history reached.'
        : 'Beginning of conversation reached.';
    }
    if (authoritative && !entries.length
      && (diagnostics?.source_truncated || diagnostics?.oversized_records || diagnostics?.corrupt_records || diagnostics?.continuation_incomplete)) {
      return 'No readable conversation messages are available in the loaded history.';
    }
    if (authoritative && entries.length && !modeEntries.length && mode === 'conversation') {
      return 'No user prompts or agent answers are recorded in the available history.';
    }
    return '';
  });
  const emptyHistoryText = $derived.by(() => {
    if (historyBusy || contextSearching || error || !authoritative) return '';
    if (!available) return reason || 'Conversation history is unavailable.';
    if (entries.length && mode === 'conversation' && !modeEntries.length) {
      return 'No user prompts or agent answers are recorded in the available history.';
    }
    if (!entries.length) {
      return diagnostics?.source_truncated || diagnostics?.oversized_records || diagnostics?.corrupt_records || diagnostics?.continuation_incomplete
        ? 'No readable conversation messages are available in the loaded history.'
        : 'No conversation messages have been recorded yet.';
    }
    return '';
  });

  onMount(() => {
    mode = localStorage.getItem('herdr-conversation-view') === 'activity' ? 'activity' : 'conversation';
    mounted = true;
    liveReader = new ConversationLiveReader({
      request: (target, revision, after, signal, subscription) => relayStore.getConversationUpdates(target, revision, after, signal, subscription),
      unsubscribe: (target, subscription) => relayStore.stopConversationUpdates(target, subscription),
      apply: (revision, incoming, sequence) => historyController?.applyLiveEntries(revision, incoming, sequence) === true,
      reset: () => historyController?.returnToLatest(),
      onState: (state, tail, capabilities) => { liveState = state; liveEntries = tail; liveCapabilities = capabilities; messageStates.retainLive(new Set(tail.map(entry => entry.id))); },
    });
    historyController = new ConversationHistoryController(agent, {
      request: async (target, request) => {
        const page = await relayStore.getConversationHistory(target, request);
        if (!request.cursor && !request.signal?.aborted) headPage = page;
        return page;
      },
      maxContentBytes: HISTORY_MAX_CONTENT_BYTES - LIVE_MAX_BYTES,
      onState: applyHistoryState,
      onInitialPage: (page) => onInitialPage?.(page),
      getPreview: (identity) => getConversationPreview(identity, Date.now(), HISTORY_WIRE_PAGE_SIZE),
      putPreview: (preview) => putConversationPreview(preview),
      isActive: () => mounted
        && document.visibilityState !== 'hidden'
        && !$securityState.locked,
    });
    controllerReady = true;
    historyController.start();
    refreshTimer = setInterval(() => {
      if (!liveReader?.observing) historyController?.refresh();
    }, refreshIntervalMs);
    const syncHistoryVisibility = () => {
      paneVisible = document.visibilityState !== 'hidden';
      if (document.visibilityState === 'hidden' || $securityState.locked) { liveReader?.pause(); historyController?.pause(); }
      else { liveReader?.resume(); historyController?.resume(); }
    };
    const selectionChanged = () => { if (selectionInHistory()) suspendFollowing(); };
    document.addEventListener('visibilitychange', syncHistoryVisibility);
    document.addEventListener('selectionchange', selectionChanged);
    return () => {
      mounted = false;
      controllerReady = false;
      if (refreshTimer) clearInterval(refreshTimer);
      refreshTimer = undefined;
      document.removeEventListener('visibilitychange', syncHistoryVisibility);
      document.removeEventListener('selectionchange', selectionChanged);
      liveReader?.stop();
      liveReader = null;
      historyController?.cancel();
      historyController = null;
    };
  });

  /**
   * Holds the view at the end of the transcript while it is pinned. Writing the
   * scroll once after a state flush is not enough: the list mounts only when
   * the loading placeholder is replaced, and the rendered markdown — wrapped
   * prose, tables, code blocks — settles its height a layout pass later still,
   * so the first readable scrollHeight is short of the final one (issue #12).
   * Every one of those moments is a size change of the stream or of the
   * viewport around it, so the observer owns the pin and re-applies it until
   * the geometry stops moving.
   */
  $effect(() => {
    const element = listElement;
    const stream = streamElement;
    if (!element || !stream || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => {
      if (pinnedToBottom) pinListToBottom(element);
    });
    // The stream grows with the turns; the scroller's own box changes with the
    // on-screen keyboard and rotation, which moves the end away as well.
    observer.observe(stream);
    observer.observe(element);
    return () => observer.disconnect();
  });

  // A streamed turn can be committed after the resize notification that
  // pinned the previous stream. Two animation frames provide a portable
  // post-layout correction for engines that deliver that notification early.
  $effect(() => {
    const window = visibleEntries;
    const list = listElement;
    if (!list || !mounted || !pinnedToBottom || query.trim()) return;
    void window;
    pinListToBottom(list);
    let firstFrame = 0;
    let secondFrame = 0;
    const pin = (force = false) => {
      if (pinnedToBottom && list === listElement && (force || pinPendingUntil > Date.now())) pinListToBottom(list);
    };
    firstFrame = requestAnimationFrame(() => {
      pin(true);
      secondFrame = requestAnimationFrame(() => pin());
    });
    return () => {
      cancelAnimationFrame(firstFrame);
      if (secondFrame) cancelAnimationFrame(secondFrame);
    };
  });

  // The loader lives inside the scroll box. A positive top margin makes the
  // sentinel a prefetch target rather than a button-sized layout item.
  $effect(() => {
    const list = listElement;
    const sentinel = topSentinel;
    const searching = query.trim();
    if (!list || !sentinel || typeof IntersectionObserver === 'undefined') return;
    const observer = new IntersectionObserver((observations) => {
      if (!observations.some((observation) => observation.isIntersecting)) return;
      if (searching || !historyController || document.visibilityState === 'hidden' || $securityState.locked) return;
      if (pinnedToBottom || !upwardDemand) return;
      demandOlder();
    }, { root: list, rootMargin: '300px 0px 0px 0px', threshold: 0 });
    observer.observe(sentinel);
    return () => observer.disconnect();
  });

  // Agent inventory updates replace the Agent object frequently. The
  // controller compares only the exact conversation target, so status/title
  // churn cannot discard a response that belongs to this conversation.
  $effect(() => {
    const currentAgent = agent;
    if (!controllerReady || !historyController) return;
    if (!historyController.matchesRequestTarget(currentAgent)) {
      liveReader?.stop();
      liveEntries = [];
      liveState = 'unavailable';
      liveCapabilities = undefined;
      headPage = null;
      renderAnchor = null;
      pendingAnchor = null;
      pinnedToBottom = true;
      upwardDemand = false;
    }
    historyController.setAgent(currentAgent);
  });

  $effect(() => {
    const locked = $securityState.locked;
    if (!controllerReady || !historyController) return;
    if (locked || document.visibilityState === 'hidden') { liveReader?.pause(); historyController.pause(); }
    else { liveReader?.resume(); historyController.resume(); }
  });

  function pinListToBottom(element: HTMLElement): void {
    if (element !== listElement || selectionInHistory()) return;
    pinTargetHeight = element.scrollHeight;
    pinTargetTop = Math.max(0, element.scrollHeight - element.clientHeight);
    pinPendingUntil = Date.now() + 250;
    if (pinPendingTimer) clearTimeout(pinPendingTimer);
    pinPendingTimer = setTimeout(() => {
      pinPendingTimer = undefined;
      pinPendingUntil = 0;
    }, 250);
    element.scrollTop = element.scrollHeight;
    lastScrollTop = element.scrollTop;
    lastScrollHeight = element.scrollHeight;
  }

  function selectionInHistory(): boolean {
    const selection = window.getSelection();
    return Boolean(selection && !selection.isCollapsed && selection.anchorNode && listElement?.contains(selection.anchorNode));
  }

  function prepareScroll(): void {
    followPendingUntil = 0;
    pinPendingUntil = 0;
  }

  function suspendFollowing(): void {
    followPendingUntil = 0;
    renderAnchor ||= visibleEntries[0]?.id || null;
    pinnedToBottom = false;
    pinPendingUntil = 0;
  }

  function resumeFollowing(): void {
    pendingAnchor = null;
    renderAnchor = null;
    pinnedToBottom = true;
    followPendingUntil = Date.now() + 250;
    if (listElement) pinListToBottom(listElement);
  }

  function demandNewer(): void {
    if (windowAdvancePending || renderStart + mountedEntryLimit >= filteredEntries.length) return;
    const anchor = topVisibleEntry();
    const top = listElement.scrollTop;
    const height = listElement.scrollHeight;
    windowAdvancePending = true;
    pendingAnchor = null;
    renderAnchor = filteredEntries[Math.min(filteredEntries.length - mountedEntryLimit, renderStart + mountedEntryLimit / 2)].id;
    void tick().then(() => {
      if (!mounted || !listElement) { windowAdvancePending = false; return; }
      restoreScrollAnchor(anchor, top, height);
      lastScrollTop = listElement.scrollTop;
      lastScrollHeight = listElement.scrollHeight;
      windowAdvancePending = false;
    });
  }

  function trackScroll() {
    if (!listElement || windowAdvancePending) return;
    if (selectionInHistory()) { suspendFollowing(); return; }
    if (followPendingUntil > Date.now() && listElement.scrollTop >= lastScrollTop - 2) {
      pinListToBottom(listElement);
      return;
    }
    followPendingUntil = 0;
    const wasPinned = pinnedToBottom;
    // Re-measured on every scroll, so a content shrink — which makes the
    // browser clamp scrollTop and fire a scroll event from a lower position —
    // lands exactly at the bottom and keeps the pin instead of dropping it.
    const bottomGap = listElement.scrollHeight - listElement.scrollTop - listElement.clientHeight;
    // WebKit can report a scroll caused by growing content before delivering
    // ResizeObserver. A larger gap alone is not reader movement: keep the pin
    // if the viewport did not move up, even after the short pin timer expires.
    const layoutDidNotScrollUp = wasPinned
      && (bottomGap < 2 || (listElement.scrollHeight >= lastScrollHeight
        && listElement.scrollTop >= lastScrollTop - 2));
    const movedTowardTail = listElement.scrollTop > lastScrollTop;
    if (bottomGap >= 2 && listElement.scrollTop < lastScrollTop - 2) upwardDemand = true;
    lastScrollTop = listElement.scrollTop;
    lastScrollHeight = listElement.scrollHeight;
    const layoutShiftFromPin = pinPendingUntil > Date.now()
      && listElement.scrollHeight >= pinTargetHeight
      && listElement.scrollTop >= pinTargetTop - 2;
    if (layoutShiftFromPin || layoutDidNotScrollUp) {
      pinnedToBottom = true;
      if (bottomGap > 1) pinListToBottom(listElement);
      return;
    }
    pinPendingUntil = 0;
    pinnedToBottom = false;
    renderAnchor ||= visibleEntries[0]?.id || null;
    if (document.visibilityState === 'hidden' || $securityState.locked) return;
    if (bottomGap < bottomThreshold) {
      if (renderStart + mountedEntryLimit < filteredEntries.length) {
        if (movedTowardTail) demandNewer();
      } else if (movedTowardTail && bottomGap < 2) resumeFollowing();
      return;
    }
    if (query.trim() || !historyController) return;
    const nearTop = listElement.scrollTop <= 300;
    if (!pinnedToBottom && nearTop && upwardDemand) demandOlder();
  }

  function applyHistoryState(next: ConversationHistoryControllerState): void {
    messageStates.setScope(`${next.identity}:${next.sourceRevision}`);
    messageStates.retain(new Set([...next.entries, ...liveEntries].map(entry => entry.id)));
    const oldEntries = entries;
    const wasLoadingOlder = loadingOlder;
    const readingAnchor = !pinnedToBottom ? topVisibleEntry() : null;
    if (!pinnedToBottom && !renderAnchor && visibleEntries.length) renderAnchor = visibleEntries[0].id;
    entries = next.entries;
    if (next.authoritative && !next.error && headPage?.sourceRevision === next.sourceRevision) {
      liveReader?.observe(agent, headPage);
      headPage.provisional = undefined;
    }
    available = next.available;
    reason = next.reason;
    hasMore = next.hasMore;
    nextCursor = next.nextCursor;
    browseState = next.state;
    browseProgress = next.progress;
    diagnostics = next.diagnostics;
    omoPlan = next.omoPlan;
    previewVisible = next.preview;
    previewHistorical = next.previewHistorical;
    authoritative = next.authoritative;
    contextSearching = next.contextSearching;
    pendingPrefix = next.pendingPrefix;
    beginningReached = next.beginningReached;
    pausedReason = next.pausedReason;
    latestGapOutstanding = next.latestGapOutstanding;
    requestPhase = next.requestPhase;
    preparationPolls = next.preparationPolls;
    error = next.error?.message || '';
    errorCode = next.errorCode;
    errorRetryable = next.errorRetryable;
    sourceChangedNotice = next.error && ['source_changed', 'invalid_cursor', 'cursor_expired'].includes(next.error.code)
      ? next.error.message
      : '';
    loadingOlder = next.requestPhase === 'older';
    loading = !entries.length && !previewVisible && next.requestPhase === 'initial';

    const prepended = oldEntries.length > 0
      && next.entries.findIndex((entry) => entry.id === oldEntries[0].id) > 0;
    if (prepended && pendingAnchor) {
      const anchor = pendingAnchor;
      const anchorIndex = filteredEntries.findIndex(entry => entry.id === anchor.anchor?.[0]);
      renderAnchor = filteredEntries[Math.max(0, anchorIndex - mountedEntryLimit / 2)]?.id || null;
      void tick().then(() => restoreScrollAnchor(anchor.anchor, anchor.top, anchor.height));
    }
    if (readingAnchor && !pendingAnchor) void tick().then(() => restoreScrollAnchor(readingAnchor));
    if (next.requestPhase === 'idle' && wasLoadingOlder) pendingAnchor = null;
  }

  function cancelHistoryRequests() {
    historyController?.cancel();
  }

  function requestLatest() {
    sourceChangedNotice = '';
    historyController?.returnToLatest();
  }

  function reloadHistory() {
    requestLatest();
  }

  function continuePreparation() {
    historyController?.continuePreparation();
  }

  function recoverHistory() {
    if (errorCode === 'preparation_stalled') historyController?.continuePreparation();
    else historyController?.retry();
  }

  function cancelPreparation() {
    historyController?.pausePreparation();
  }

  type PendingScrollAnchor = { anchor: ScrollAnchor | null; top: number; height: number };
  let pendingAnchor: PendingScrollAnchor | null = null;

  function demandOlder() {
    if (windowAdvancePending) return;
    upwardDemand = false;
    if (renderStart > 0) {
      const anchor = topVisibleEntry();
      const top = listElement?.scrollTop || 0;
      const height = listElement?.scrollHeight || 0;
      suspendFollowing();
      windowAdvancePending = true;
      renderAnchor = filteredEntries[Math.max(0, renderStart - mountedEntryLimit / 2)]?.id || null;
      void tick().then(() => {
        if (!mounted || !listElement) { windowAdvancePending = false; return; }
        restoreScrollAnchor(anchor, top, height);
        const bottom = listElement.scrollHeight - listElement.clientHeight;
        if (listElement.scrollTop >= bottom - 2) listElement.scrollTop = Math.max(0, bottom - 8);
        lastScrollTop = listElement.scrollTop;
        lastScrollHeight = listElement.scrollHeight;
        windowAdvancePending = false;
      });
      return;
    }
    if (!mounted || !historyController || !nextCursor || historyBusy || error) return;
    pendingAnchor = {
      anchor: topVisibleEntry(),
      top: listElement?.scrollTop || 0,
      height: listElement?.scrollHeight || 0,
    };
    pinnedToBottom = false;
    historyController.demandOlder();
  }

  function loadOlder() {
    demandOlder();
  }
  function toggleSpeech(text: string): void {
    if ($speechState === 'speaking') {
      stopSpeech();
      return;
    }
    const toast = (message: string) => relayStore.showToast(message, true);
    // Checked before anything plays: unlocking audio for a language the relay
    // cannot speak leaves the phone with a silent stream and no explanation.
    const languages = $connections.get(agent.relay_id)?.speechLanguages ?? [];
    if (!languages.includes($speechLanguage)) {
      toast(`This relay has no ${speechLanguageLabel($speechLanguage)} voice; install a Piper voice for it on that computer.`);
      return;
    }
    // Armed inside the tap: the relay fetches audio before playing, and a
    // play() after that round trip is autoplay-blocked.
    armSpeechKeepalive(toast);
    const spoke = speakViaRelay(
      text,
      (chunk, language) => relayStore.speakToAgent(agent, chunk, language),
      toast,
    );
    if (!spoke) releaseSpeechKeepalive();
  }


  type ScrollAnchor = readonly [string, HTMLElement, number, HTMLElement[]];

  function topVisibleEntry(): ScrollAnchor | null {
    if (!listElement) return null;
    const listTop = listElement.getBoundingClientRect().top;
    const candidates = [...listElement.querySelectorAll<HTMLElement>('.conversation-entry')];
    const index = candidates.findIndex((element) => element.getBoundingClientRect().bottom > listTop);
    if (index < 0) return null;
    const candidate = candidates[index];
    const fallbacks = candidates.slice(index + 1).concat(candidates.slice(0, index).reverse());
    return [candidate.dataset.conversationEntryId || '', candidate, candidate.getBoundingClientRect().top - listTop, fallbacks];
  }

  function restoreScrollAnchor(anchor: ScrollAnchor | null, previousTop = 0, previousHeight = 0): void {
    if (!listElement || pinnedToBottom) return;
    const byId = anchor?.[0]
      ? [...listElement.querySelectorAll<HTMLElement>('.conversation-entry')]
        .find((element) => element.dataset.conversationEntryId === anchor[0])
      : undefined;
    const candidate = anchor && (anchor[1].isConnected ? anchor[1] : byId || anchor[3].find((element) => element.isConnected));
    if (anchor && candidate) {
      const listTop = listElement.getBoundingClientRect().top;
      listElement.scrollTop += candidate.getBoundingClientRect().top - listTop - anchor[2];
      return;
    }
    listElement.scrollTop = previousTop + listElement.scrollHeight - previousHeight;
  }

  function continuationMessage(): string {
    switch (diagnostics?.continuation_reason) {
      case 'resolution_limit':
        return 'This conversation may continue, but its continuation chain could not be fully checked. Reload to try again.';
      case 'invalid_link':
      case 'ambiguous_link':
      case 'cycle':
      case 'missing_source':
      default:
        return 'This conversation continues in another session, but part of that history is unavailable. Reload to try again.';
    }
  }

  function setMode(next: 'conversation' | 'activity') {
    windowAdvancePending = true;
    renderAnchor = null;
    mode = next;
    localStorage.setItem('herdr-conversation-view', next);
    void tick().then(() => {
      if (listElement) {
        lastScrollTop = listElement.scrollTop;
        lastScrollHeight = listElement.scrollHeight;
      }
      windowAdvancePending = false;
    });
  }

  function formatTimestamp(value: string): string {
    const timestamp = new Date(value);
    if (Number.isNaN(timestamp.getTime())) return '';
    return timestamp.toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });
  }

  async function copyMarkdown(entry: ConversationEntry) {
    if (!entry.text || !navigator.clipboard?.writeText) {
      relayStore.showToast('Clipboard access is unavailable. Select the text manually.', true);
      return;
    }
    try {
      await navigator.clipboard.writeText(entry.text);
      relayStore.showToast('Markdown copied.');
    } catch {
      relayStore.showToast('Could not copy. Select it manually.', true);
    }
  }
  async function copyCode(code: string) {
    if ($securityState.locked || !navigator.clipboard?.writeText) {
      relayStore.showToast('Clipboard access is unavailable while the app is locked.', true);
      return;
    }
    try {
      await navigator.clipboard.writeText(code);
      relayStore.showToast('Code copied.');
    } catch {
      relayStore.showToast('Could not copy. Select it manually.', true);
    }
  }


  onDestroy(() => {
    messageStates.clear();
    liveReader?.stop();
    if (pinPendingTimer) clearTimeout(pinPendingTimer);
    cancelHistoryRequests();
  });

</script>

<main class="conversation-page" aria-labelledby="conversation-title">
  <header class="conversation-toolbar">
    <div>
      <h2 id="conversation-title">Conversation</h2>
      {#if branchUnverified}<small class="hint conversation-branch-trust">Branch unverified<span class="sr-only">: saved history may not reflect the current branch.</span></small>{/if}
    </div>
    <div class="conversation-toolbar-actions">
      <div class="conversation-mode" role="group" aria-label="Conversation display">
        <button class:active={mode === 'conversation'} type="button" aria-pressed={mode === 'conversation'} title="Show user prompts and the latest agent answer from each exchange" onclick={() => setMode('conversation')}>Conversation</button>
        <button class:active={mode === 'activity'} type="button" aria-pressed={mode === 'activity'} title="Show every recorded agent message and tool call" onclick={() => setMode('activity')}>Full history</button>
      </div>
      {#if entries.length}
        <label class="conversation-search">
          <span class="sr-only">Search loaded conversation</span>
          <input type="search" bind:value={query} placeholder="Search" />
        </label>
      {/if}
    </div>
  </header>
  {#if liveStatusText}<p class="conversation-source-status" role="status">{liveStatusText}</p>{/if}
  {#if readOnly}
    <p class="conversation-warning" role="status">Reader access is read only. Use a controller device to reply.</p>
  {/if}
  {#if previewVisible && previewHistorical && !authoritative}
    <p class="conversation-warning" role="status">Saved history is shown while current messages are checked.</p>
  {/if}

  {#if loading && !entries.length}
    <div class="empty-state" role="status" aria-live="polite">{historyStatusText || 'Loading conversation…'}</div>
  {:else if !available && !entries.length && authoritative}
    <div class="empty-state" role="status">{reason || 'Conversation history is unavailable.'}</div>
  {:else}
    {#if sourceChangedNotice}
      <p class="conversation-warning error" role="alert">
        {sourceChangedNotice}
        <Button variant="secondary" size="sm" onclick={reloadHistory}>Reload history</Button>
      </p>
    {/if}
    {#if hasMore && !sourceChangedNotice && preparationPolls < maxPreparationPolls && browseState !== 'preparing'}
      <div class="conversation-older">
        <Button variant="secondary" size="sm" disabled={loadingOlder || preparationPolls >= maxPreparationPolls} onclick={loadOlder}>
          {loadingOlder ? 'Loading…' : browseState === 'failed' ? 'Retry loading' : 'Load older turns'}
        </Button>
      </div>
    {/if}
    {#if nextCursor && (preparationPolls >= maxPreparationPolls || browseState === 'preparing')}
      <p class="conversation-warning" role="status">
        {#if preparationPolls >= maxPreparationPolls}
          Preparation is paused. <Button variant="secondary" size="sm" onclick={continuePreparation}>Continue</Button>
        {:else}
          Preparing history ({browseProgress?.phase || 'scanning'}){#if browseProgress?.source_bytes} — {Math.min(100, Math.round((browseProgress.scanned_bytes / browseProgress.source_bytes) * 100))}% scanned{/if}…
          <Button variant="secondary" size="sm" onclick={cancelPreparation}>Cancel</Button>
        {/if}
      </p>
    {/if}
    {#if diagnostics?.continuation_incomplete}
      <p class="conversation-warning error" role="status">
        {continuationMessage()}
        <Button variant="secondary" size="sm" onclick={reloadHistory}>Reload history</Button>
      </p>
    {/if}
    {#if diagnostics?.oversized_records}
      <p class="conversation-warning" role="status">{diagnostics.oversized_records} oversized {diagnostics.oversized_records === 1 ? 'record was' : 'records were'} skipped from the full history.</p>
    {/if}
    {#if diagnostics?.omitted_tools || diagnostics?.omitted_payloads}
      <p class="conversation-warning" role="status">Some tool activity is shortened to keep this history page within its response limit{#if diagnostics?.omitted_tools} ({diagnostics.omitted_tools} tool{diagnostics.omitted_tools === 1 ? '' : 's'} omitted){/if}{#if diagnostics?.omitted_payloads}; {diagnostics.omitted_payloads} payload{diagnostics.omitted_payloads === 1 ? '' : 's'} shortened{/if}.</p>
    {/if}
    {#if diagnostics?.corrupt_records || diagnostics?.plan_corrupt}
      <p class="conversation-warning error" role="status">Some records could not be decoded. Valid turns are shown, but the source may be damaged.</p>
    {/if}
    {#if error && !sourceChangedNotice}
      <p class="conversation-warning error" role="alert">
        {error}
        {#if errorRetryable}<Button variant="secondary" size="sm" onclick={recoverHistory}>{['work_deadline', 'work_limit', 'stalled', 'preparation_stalled'].includes(errorCode) ? 'Continue' : 'Retry'}</Button>{/if}
      </p>
    {/if}
    {#if omoPlan}<OmoPlan plan={omoPlan} />{/if}
    {#if emptyHistoryText}
      <div class="empty-state" role="status">{emptyHistoryText}</div>
    {/if}
    {#if query.trim() && !visibleEntries.length && entries.length}
      <div class="empty-state" role="status">No loaded turns match “{query.trim()}”.</div>
    {/if}
    <section
      class="conversation-list"
      bind:this={listElement}
      onscroll={trackScroll}
      onpointerdown={prepareScroll}
      onfocusin={prepareScroll}
      aria-label={`Conversation with ${agentName}`}
      aria-busy={historyBusy}
    >
      <p class="conversation-history-status" role="status" aria-live="polite" aria-hidden={!historyStatusText}>
        {historyStatusText || '\u00a0'}
      </p>
      <div class="conversation-history-sentinel" bind:this={topSentinel} aria-hidden="true"></div>
      {#if renderStart > 0}
        <Button variant="secondary" size="sm" onclick={demandOlder}>Show earlier loaded messages</Button>
      {/if}
      {#if filteredEntries.length > mountedEntryLimit}
        <p class="conversation-source-status">Showing {renderStart + 1}–{Math.min(filteredEntries.length, renderStart + mountedEntryLimit)} of {filteredEntries.length} loaded messages. Search covers loaded history.</p>
      {/if}
      <div class="conversation-stream" bind:this={streamElement}>
        {#each visibleEntries as entry (entry.id)}
          {@const code = fencedCodeText(entry.text)}
          {@const timestamp = formatTimestamp(entry.timestamp)}
          <article
            class:conversation-user={entry.role === 'user'}
            class:conversation-notice={entry.kind === 'notice'}
            class="conversation-entry"
            data-conversation-entry-id={entry.id}
          >
            <header>
              <strong>{entry.kind === 'notice' ? 'Extension notice' : entry.role === 'user' ? 'You' : agentName}</strong>
              <span class="conversation-entry-actions">
                {#if timestamp}<time datetime={entry.timestamp}>{timestamp}</time>{/if}
                {#if entry.role === 'assistant' && entry.text && $speechEnabled}
                  <Button
                    variant="ghost"
                    size="sm"
                    aria-label={$speechState === 'speaking' ? 'Stop reading response' : 'Read response aloud'}
                    title={$speechState === 'speaking' ? 'Stop reading' : `Read aloud in ${speechLanguageLabel($speechLanguage)}`}
                    onclick={() => toggleSpeech(entry.text)}
                  >{$speechState === 'speaking' ? 'Stop' : 'Speak'}</Button>
                {/if}
                {#if entry.text}
                  <Button
                    class="copy-conversation-markdown"
                    variant="ghost"
                    size="icon"
                    aria-label={`Copy ${entry.role === 'user' ? 'your' : agentName} message as Markdown`}
                    title="Copy Markdown"
                    onclick={() => copyMarkdown(entry)}
                  >
                    <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                      <rect x="9" y="9" width="13" height="13" rx="2"></rect>
                      <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path>
                    </svg>
                  </Button>
                {/if}
                {#if code}
                  <Button
                    class="copy-conversation-code"
                    variant="ghost"
                    size="icon"
                    disabled={$securityState.locked}
                    aria-label={`Copy code from ${entry.role === 'user' ? 'your' : agentName} message`}
                    title="Copy code"
                    onclick={() => copyCode(code)}
                  >&lt;/&gt;</Button>
                {/if}
              </span>
            </header>
            <ConversationMessage viewState={messageStates.get(entry.id)} text={entry.text} tools={entry.tools} highlight={query.trim()} />
            {#if entry.id.startsWith('live:')}<small>Live · not saved yet</small>{/if}
            {#if entry.truncated}<small>Long turn truncated by the relay.</small>{/if}
          </article>
        {/each}
      </div>
    </section>
  {/if}
  <div class="conversation-input-area">
    {#if !readOnly && delivery.uncertain}
      <p role="alert">Delivery is uncertain. You can edit drafts; open Terminal and use Type in terminal to inspect before sending.</p>
    {:else if !readOnly && delivery.pending}
      <p role="status">Waiting for terminal input delivery.</p>
    {/if}
    <AgentInteraction {agent} {readOnly} responding={responding.has(agent.pane_id)} blocked={inputBlocked} />
    {#key JSON.stringify(targetRefForAgent(agent))}
      <PromptComposer {agent} {frame} {readOnly}
        dispatchBlocked={inputBlocked ? 'Check terminal input delivery in Terminal before sending.' : draftState.secretReady ? '' : dispatchBlocked}
        {noEchoActive} {draftPersistenceAllowed} noEchoPrompt={frame?.noEchoPrompt || 'Password:'}
        placeholder={inputPlaceholder} onsubmitted={() => { historyController?.refresh(); }} />
    {/key}
    {#if readOnly}
      <p class="conversation-composer-status" role="status">Reader access is read only. Use a controller device to send input or answer prompts.</p>
    {/if}
  </div>
</main>

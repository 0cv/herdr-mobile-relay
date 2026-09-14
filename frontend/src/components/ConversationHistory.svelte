<script lang="ts">
  import { onDestroy, onMount, tick, untrack } from 'svelte';
  import AttachmentProgress from '$components/AttachmentProgress.svelte';
  import ConversationMessage from '$components/ConversationMessage.svelte';
  import OmoPlan from '$components/OmoPlan.svelte';
  import Button from '$components/ui/Button.svelte';
  import { agentNeedsInspection, agentNeedsResponse, displayName } from '$lib/agents';
  import { conversationEntries } from '$lib/conversation';
  import {
    ConversationHistoryController,
    type ConversationHistoryControllerState,
    HISTORY_MAX_PREPARATION_POLLS,
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
  import { securityState } from '$lib/security';
  import { clearPromptDraft, loadPromptDraft, savePromptDraft } from '$lib/prompt-drafts';
  import { relayStore } from '$lib/store';
  import type { AttachmentBatchController, AttachmentBatchSnapshot, AttachmentRef } from '$lib/attachments';
  import type { Agent, ConversationEntry, ConversationPage, OmoTodoState } from '$lib/types';

  let {
    agent,
    readOnly = false,
    onInitialPage,
  }: { agent: Agent; readOnly?: boolean; onInitialPage?: (page: ConversationPage) => void } = $props();

  const connections = relayStore.connections;

  let entries = $state<ConversationEntry[]>([]);
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
  let composerElement = $state<HTMLTextAreaElement>(null!);
  let fileInput = $state<HTMLInputElement>(null!);
  let imageInput = $state<HTMLInputElement>(null!);
  let composer = $state(untrack(() => loadPromptDraft(agent)));
  let sendingPrompt = $state(false);
  let uploadingAttachment = $state(false);
  let uploadStatus = $state('');
  let uploadError = $state(false);
  let attachmentController = $state<AttachmentBatchController | null>(null);
  let attachmentSnapshot = $state<AttachmentBatchSnapshot | null>(null);
  let attachmentUnsubscribe: (() => void) | null = null;
  let attachmentCancelRequested = false;
  /**
   * Whether the view follows the end of the transcript. It starts pinned so
   * opening a session lands on the newest turn, and only the reader scrolling
   * away from the bottom releases it.
   */
  let pinnedToBottom = $state(true);
  let pinPendingUntil = 0;
  let pinTargetTop = 0;
  let pinTargetHeight = 0;
  let lastScrollTop = 0;
  let lastScrollHeight = 0;
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
    if (!pendingPrefix.length) return compact;
    const pendingIds = new Set(pendingPrefix.map((entry) => entry.id));
    return compact.filter((entry) => !pendingIds.has(entry.id));
  });
  const inputLocked = $derived(readOnly || agentNeedsResponse(agent) || agentNeedsInspection(agent));
  const inputPlaceholder = $derived(readOnly
    ? 'Reader access is read only'
    : agentNeedsResponse(agent)
      ? 'Needs response — switch to Terminal'
      : agentNeedsInspection(agent)
        ? 'Needs inspection — switch to Terminal'
        : 'Type a reply…');
  const visibleEntries = $derived.by(() => {
    const needle = query.trim().toLocaleLowerCase();
    if (!needle) return modeEntries;
    return modeEntries.filter((entry) => `${entry.text} ${(entry.tools || []).map((tool) => `${tool.name} ${tool.input || ''} ${tool.output || ''}`).join(' ')}`.toLocaleLowerCase().includes(needle));
  });
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
    historyController = new ConversationHistoryController(agent, {
      request: (target, request) => relayStore.getConversationHistory(target, request),
      onState: applyHistoryState,
      onInitialPage: (page) => onInitialPage?.(page),
      getPreview: (identity) => getConversationPreview(identity),
      putPreview: (preview) => putConversationPreview(preview),
      isActive: () => mounted
        && document.visibilityState !== 'hidden'
        && !$securityState.locked
        && navigator.onLine !== false,
    });
    controllerReady = true;
    historyController.start();
    refreshTimer = setInterval(() => {
      historyController?.refresh();
    }, refreshIntervalMs);
    const syncHistoryVisibility = () => {
      if (document.visibilityState === 'hidden' || $securityState.locked) historyController?.pause();
      else historyController?.resume();
    };
    document.addEventListener('visibilitychange', syncHistoryVisibility);
    return () => {
      mounted = false;
      controllerReady = false;
      if (refreshTimer) clearInterval(refreshTimer);
      refreshTimer = undefined;
      document.removeEventListener('visibilitychange', syncHistoryVisibility);
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

  $effect(() => {
    const value = composer;
    void tick().then(() => {
      if (value === composer) resizeComposer();
    });
  });

  // A streamed turn can be committed after the resize notification that
  // pinned the previous stream. Two animation frames provide a portable
  // post-layout correction for engines that deliver that notification early.
  $effect(() => {
    const count = entries.length;
    const list = listElement;
    if (!list || !mounted || !pinnedToBottom || query.trim()) return;
    void count;
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
      if (pinnedToBottom) {
        if (list.scrollHeight <= list.clientHeight + 8) historyController.ensureMore();
        return;
      }
      demandOlder();
    }, { root: list, rootMargin: '300px 0px 0px 0px', threshold: 0 });
    observer.observe(sentinel);
    return () => observer.disconnect();
  });

  // IntersectionObserver does not fire usefully for an empty or underfilled
  // list in every mobile engine. Recheck after layout; the fallback remains a
  // distance test in trackScroll when an observer is unavailable.
  $effect(() => {
    const count = entries.length;
    const list = listElement;
    if (!list || !mounted || query.trim() || list.clientHeight <= 0) return;
    void count;
    const timer = setTimeout(() => {
      if (list.scrollHeight <= list.clientHeight + 8 && pinnedToBottom) historyController?.ensureMore();
    }, 0);
    return () => clearTimeout(timer);
  });

  // Agent inventory updates replace the Agent object frequently. The
  // controller compares only the exact conversation target, so status/title
  // churn cannot discard a response that belongs to this conversation.
  $effect(() => {
    const currentAgent = agent;
    if (!controllerReady || !historyController) return;
    historyController.setAgent(currentAgent);
  });

  $effect(() => {
    const locked = $securityState.locked;
    if (!controllerReady || !historyController) return;
    if (locked || document.visibilityState === 'hidden') historyController.pause();
    else historyController.resume();
  });

  // The same per-agent draft store TerminalView uses, so a reply drafted here
  // survives switching views or panes and continues in the terminal composer.
  $effect(() => {
    savePromptDraft(agent, composer);
  });

  function pinListToBottom(element: HTMLElement): void {
    if (element !== listElement) return;
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

  function trackScroll() {
    if (!listElement) return;
    const wasPinned = pinnedToBottom;
    // Re-measured on every scroll, so a content shrink — which makes the
    // browser clamp scrollTop and fire a scroll event from a lower position —
    // lands exactly at the bottom and keeps the pin instead of dropping it.
    const bottomGap = listElement.scrollHeight - listElement.scrollTop - listElement.clientHeight;
    // WebKit can report a scroll caused by growing content before delivering
    // ResizeObserver. A larger gap alone is not reader movement: keep the pin
    // if the viewport did not move up, even after the short pin timer expires.
    const layoutDidNotScrollUp = wasPinned
      && listElement.scrollHeight >= lastScrollHeight
      && listElement.scrollTop >= lastScrollTop - 2;
    lastScrollTop = listElement.scrollTop;
    lastScrollHeight = listElement.scrollHeight;
    const layoutShiftFromPin = pinPendingUntil > Date.now()
      && listElement.scrollHeight >= pinTargetHeight
      && listElement.scrollTop >= pinTargetTop - 2;
    if (layoutShiftFromPin || layoutDidNotScrollUp) {
      pinnedToBottom = true;
      return;
    }
    pinPendingUntil = 0;
    pinnedToBottom = bottomGap < 48;
    if (query.trim() || !historyController || document.visibilityState === 'hidden' || $securityState.locked) return;
    const nearTop = listElement.scrollTop <= 300;
    if (!pinnedToBottom && nearTop) demandOlder();
    else if (wasPinned && nearTop && listElement.clientHeight > 0 && listElement.scrollHeight <= listElement.clientHeight + 8) {
      historyController.ensureMore();
    }
  }

  function applyHistoryState(next: ConversationHistoryControllerState): void {
    const oldEntries = entries;
    entries = next.entries;
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
      void tick().then(() => restoreScrollAnchor(anchor.anchor, anchor.top, anchor.height));
    }
    if (next.requestPhase === 'idle') pendingAnchor = null;
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
    return [visibleEntries[index]?.id || '', candidate, candidate.getBoundingClientRect().top - listTop, fallbacks];
  }

  function restoreScrollAnchor(anchor: ScrollAnchor | null, previousTop = 0, previousHeight = 0): void {
    if (!listElement) return;
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
    mode = next;
    localStorage.setItem('herdr-conversation-view', next);
    historyController?.setMode(next);
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


  function resizeComposer() {
    if (!composerElement) return;
    composerElement.style.height = 'auto';
    const maxHeight = Number.parseFloat(getComputedStyle(composerElement).maxHeight);
    const contentHeight = composerElement.scrollHeight;
    const capped = Number.isFinite(maxHeight) && contentHeight > maxHeight;
    composerElement.style.height = `${capped ? maxHeight : contentHeight}px`;
    composerElement.style.overflowY = capped ? 'auto' : 'hidden';
  }

  function clearUploadStatus() {
    uploadStatus = '';
    uploadError = false;
  }

  function clearComposer() {
    composer = '';
    clearUploadStatus();
  }

  function composerKeydown(event: KeyboardEvent) {
    if (event.isComposing) return;
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void sendPrompt();
    }
  }

  async function sendPrompt() {
    const submittedDraft = composer;
    const text = submittedDraft.replace(/[\r\n]+$/g, '');
    if (!text || inputLocked || sendingPrompt || uploadingAttachment) return;
    sendingPrompt = true;
    composer = '';
    clearPromptDraft(agent);
    try {
      await relayStore.sendToAgent(agent, { type: 'submit_prompt', text });
      relayStore.showToast('Prompt sent.');
      clearUploadStatus();
      setTimeout(() => {
        if (!mounted) return;
        historyController?.refresh();
      }, 500);
    } catch (failure) {
      const dispatchedUnknown = typeof failure === 'object'
        && failure !== null
        && 'data' in failure
        && typeof failure.data === 'object'
        && failure.data !== null
        && 'dispatched_unknown' in failure.data
        && failure.data.dispatched_unknown === true;
      if (!composer && !dispatchedUnknown) composer = submittedDraft;
      else clearUploadStatus();
      const detail = failure instanceof Error ? failure.message : 'Prompt could not be sent.';
      relayStore.showToast(
        dispatchedUnknown ? `${detail} Check the terminal before sending again.` : detail,
        true,
      );
    } finally {
      sendingPrompt = false;
    }
  }

  function appendUploadedAttachments(attachments: AttachmentRef[]): void {
    const rejected = attachmentSnapshot?.items.filter((item) => item.state === 'rejected') || [];
    if (!attachments.length) {
      uploadStatus = attachmentCancelRequested
        ? 'Attachment upload canceled.'
        : rejected.length
          ? 'No selected attachments passed validation.'
          : 'No attachments were uploaded.';
      uploadError = !attachmentCancelRequested;
      return;
    }
    const prefix = composer && !composer.endsWith('\n') ? '\n' : '';
    composer += `${prefix}${attachments.map((attachment) => `Attachment: ${attachment.ref}`).join('\n')}\n`;
    uploadStatus = `Attached ${attachments.map((attachment) => attachment.name).join(', ')}${rejected.length ? `; ${rejected.length} rejected` : ''}`;
    uploadError = rejected.length > 0;
    if (!rejected.length) attachmentSnapshot = null;
  }

  function releaseAttachmentController(controller: AttachmentBatchController, force = false): void {
    if (!force && attachmentSnapshot?.items.some((item) => item.state === 'interrupted')) return;
    attachmentUnsubscribe?.();
    attachmentUnsubscribe = null;
    if (attachmentController === controller) attachmentController = null;
  }

  async function filesSelected(files: FileList | File[]) {
    const selected = [...files];
    if (!selected.length || inputLocked || sendingPrompt || uploadingAttachment) return;
    uploadingAttachment = true;
    uploadStatus = `Uploading ${selected.length} attachment${selected.length === 1 ? '' : 's'}…`;
    uploadError = false;
    attachmentCancelRequested = false;
    let controller: AttachmentBatchController | null = null;
    try {
      const previous = attachmentController;
      if (previous) {
        try {
          await previous.cancel();
        } finally {
          releaseAttachmentController(previous, true);
        }
      }
      controller = relayStore.attachmentController(agent);
      attachmentController = controller;
      attachmentUnsubscribe?.();
      attachmentUnsubscribe = controller.subscribe((snapshot) => {
        attachmentSnapshot = snapshot;
      });
      controller.select(selected);
      const attachments = await controller.upload();
      appendUploadedAttachments(attachments);
    } catch (failure) {
      uploadStatus = attachmentCancelRequested
        ? 'Attachment upload canceled.'
        : failure instanceof Error && failure.message
          ? failure.message
          : 'Attachments could not be uploaded.';
      uploadError = !attachmentCancelRequested;
    } finally {
      uploadingAttachment = false;
      if (controller) releaseAttachmentController(controller);
    }
  }
  async function restartAttachmentUpload(): Promise<void> {
    const controller = attachmentController;
    if (!controller || uploadingAttachment) return;
    uploadingAttachment = true;
    uploadStatus = 'Restarting interrupted files from the beginning…';
    uploadError = false;
    attachmentCancelRequested = false;
    try {
      appendUploadedAttachments(await controller.restart());
    } catch (failure) {
      uploadStatus = failure instanceof Error && failure.message
        ? failure.message
        : 'Attachments could not be restarted.';
      uploadError = true;
    } finally {
      uploadingAttachment = false;
      releaseAttachmentController(controller);
    }
  }


  async function cancelAttachmentUpload(): Promise<void> {
    const controller = attachmentController;
    if (!controller) return;
    attachmentCancelRequested = true;
    try {
      await controller.cancel();
      attachmentSnapshot = null;
    } catch {
      uploadStatus = 'The relay could not confirm attachment cancellation.';
      uploadError = true;
    } finally {
      releaseAttachmentController(controller, true);
    }
  }

  onDestroy(() => {
    if (pinPendingTimer) clearTimeout(pinPendingTimer);
    attachmentUnsubscribe?.();
    void attachmentController?.cancel();
    cancelHistoryRequests();
  });

  function paste(event: ClipboardEvent) {
    const files = [...(event.clipboardData?.items || [])]
      .filter((item) => item.kind === 'file' && item.type.startsWith('image/'))
      .map((item) => item.getAsFile())
      .filter((file): file is File => Boolean(file));
    if (!files.length) return;
    event.preventDefault();
    void filesSelected(files);
  }
</script>

<main class="conversation-page" aria-labelledby="conversation-title">
  <header class="conversation-toolbar">
    <div>
      <h2 id="conversation-title">Conversation</h2>
    </div>
    <div class="conversation-toolbar-actions">
      <div class="conversation-mode" role="group" aria-label="Conversation display">
        <button class:active={mode === 'conversation'} type="button" aria-pressed={mode === 'conversation'} title="Show user prompts and the latest agent answer from each exchange" onclick={() => setMode('conversation')}>Conversation</button>
        <button class:active={mode === 'activity'} type="button" aria-pressed={mode === 'activity'} title="Show every recorded agent message and tool call" onclick={() => setMode('activity')}>Full history</button>
      </div>
      {#if entries.length}
        <label class="conversation-search">
          <span class="sr-only">Search displayed conversation</span>
          <input type="search" bind:value={query} placeholder="Search" />
        </label>
      {/if}
    </div>
  </header>
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
    {#if hasMore && !sourceChangedNotice && preparationPolls < maxPreparationPolls && browseState !== 'preparing' && typeof IntersectionObserver === 'undefined'}
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
      aria-label={`Conversation with ${agentName}`}
      aria-busy={historyBusy}
    >
      <p class="conversation-history-status" role="status" aria-live="polite" aria-hidden={!historyStatusText}>
        {historyStatusText || '\u00a0'}
      </p>
      <div class="conversation-history-sentinel" bind:this={topSentinel} aria-hidden="true"></div>
      <div class="conversation-stream" bind:this={streamElement}>
        {#each visibleEntries as entry (entry.id)}
          {@const code = fencedCodeText(entry.text)}
          {@const timestamp = formatTimestamp(entry.timestamp)}
          <article
            class:conversation-user={entry.role === 'user'}
            class="conversation-entry"
            data-conversation-entry-id={entry.id}
          >
            <header>
              <strong>{entry.role === 'user' ? 'You' : agentName}</strong>
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
            <ConversationMessage messageId={entry.id} text={entry.text} tools={entry.tools} highlight={query.trim()} />
            {#if entry.truncated}<small>Long turn truncated by the relay.</small>{/if}
          </article>
        {/each}
      </div>
    </section>
  {/if}

  <div class="conversation-input-area">
    <form
      class="conversation-composer"
      aria-label="Send a prompt"
      aria-busy={sendingPrompt || uploadingAttachment}
      onsubmit={(event) => { event.preventDefault(); void sendPrompt(); }}
    >
      <!-- Images get their own input: a mixed accept list makes Android offer
           the generic file picker instead of the photo picker, hiding
           screenshots behind a Files detour. -->
      <div class="attach-stack">
      <Button
        variant="ghost"
        size="icon"
        disabled={inputLocked || uploadingAttachment || sendingPrompt}
        aria-label="Attach photos"
        onclick={() => imageInput.click()}
      >
        <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
          <rect x="3" y="4" width="18" height="16" rx="2"></rect>
          <circle cx="8.5" cy="9" r="1.5"></circle>
          <path d="m4 17 4.5-4.5 3.5 3.5 2.5-2.5L20 19"></path>
        </svg>
      </Button>
      <Button
        variant="ghost"
        size="icon"
        disabled={inputLocked || uploadingAttachment || sendingPrompt}
        aria-label="Attach files"
        onclick={() => fileInput.click()}
      >
        <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
          <path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48"></path>
        </svg>
      </Button>
      </div>
      <div class:has-text={Boolean(composer)} class="composer-field">
        <textarea
          bind:this={composerElement}
          bind:value={composer}
          rows="1"
          disabled={inputLocked}
          placeholder={inputPlaceholder}
          aria-label="Prompt"
          autocomplete="off"
          autocorrect="on"
          autocapitalize="sentences"
          spellcheck="true"
          enterkeyhint="enter"
          onkeydown={composerKeydown}
          onpaste={paste}
        ></textarea>
        {#if composer}<button type="button" class="input-clear" aria-label="Clear prompt text" onclick={clearComposer}>×</button>{/if}
      </div>
      <Button
        type="submit"
        size="icon"
        disabled={!composer.replace(/[\r\n]+$/g, '') || inputLocked || sendingPrompt || uploadingAttachment}
        aria-label={sendingPrompt ? 'Submitting input' : 'Send prompt'}
      >{sendingPrompt ? '…' : '➤'}</Button>
      <input
        bind:this={imageInput}
        type="file"
        accept="image/*"
        multiple
        hidden
        onchange={(event) => { void filesSelected(event.currentTarget.files || []); event.currentTarget.value = ''; }}
      />
      <input
        bind:this={fileInput}
        type="file"
        accept="image/png,image/jpeg,image/gif,image/webp,text/plain,text/markdown,text/csv,application/json,application/pdf,.docx,.xlsx,.pptx,.odt,.ods,.odp"
        multiple
        hidden
        onchange={(event) => { void filesSelected(event.currentTarget.files || []); event.currentTarget.value = ''; }}
      />
    </form>
    {#if attachmentSnapshot?.items.length}
      <AttachmentProgress snapshot={attachmentSnapshot} oncancel={cancelAttachmentUpload} onrestart={restartAttachmentUpload} />
    {/if}
    {#if inputLocked}
      <p class="conversation-composer-status" role="status">Switch to Terminal to handle the pending agent interaction.</p>
    {:else if uploadStatus}
      <p class:error={uploadError} class="conversation-composer-status" role="status">{uploadStatus}</p>
    {/if}
  </div>
</main>

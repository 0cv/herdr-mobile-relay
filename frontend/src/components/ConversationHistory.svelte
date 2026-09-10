<script lang="ts">
  import { onDestroy, onMount, tick, untrack } from 'svelte';
  import AttachmentProgress from '$components/AttachmentProgress.svelte';
  import ConversationMessage from '$components/ConversationMessage.svelte';
  import OmoPlan from '$components/OmoPlan.svelte';
  import Button from '$components/ui/Button.svelte';
  import { agentNeedsInspection, agentNeedsResponse, displayName } from '$lib/agents';
  import { conversationEntries } from '$lib/conversation';
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
  let total = $state<number | null>(null);
  let nextCursor = $state('');
  let browseState = $state<ConversationPage['state']>('ready');
  let browseProgress = $state<ConversationPage['progress']>();
  let sourceRevision = $state('');
  let snapshotId = $state('');
  let olderBrowsing = $state(false);
  let snapshotActive = $state(false);
  let sourceChangedNotice = $state('');
  let snapshotSendNotice = $state('');
  let diagnostics = $state<ConversationPage['diagnostics']>();
  let omoPlan = $state<OmoTodoState | null>(null);
  let loading = $state(true);
  let loadingOlder = $state(false);
  let error = $state('');
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
  let mounted = false;
  let initialLoadSettled = false;
  let latestRequestToken = 0;
  let olderRequestToken = 0;
  let preparationPolls = $state(0);
  let refreshTimer: ReturnType<typeof setInterval> | undefined;
  let latestController: AbortController | undefined;
  let olderController: AbortController | undefined;
  const refreshIntervalMs = 5_000;
  const preparationIntervalMs = 1_000;
  const maxPreparationPolls = 30;

  const agentName = $derived(displayName(agent));
  const modeEntries = $derived(mode === 'conversation' ? conversationEntries(entries) : entries);
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

  onMount(() => {
    mode = localStorage.getItem('herdr-conversation-view') === 'activity' ? 'activity' : 'conversation';
    mounted = true;
    void loadLatest();
    refreshTimer = setInterval(() => {
      if (snapshotActive || sourceChangedNotice || browseState === 'failed' || preparationPolls >= maxPreparationPolls) return;
      if (document.visibilityState === 'hidden' || $securityState.locked) return;
      void loadLatest();
    }, refreshIntervalMs);
    const resumePreparation = () => {
      if (browseState === 'preparing' && !$securityState.locked) void loadLatest();
    };
    document.addEventListener('visibilitychange', resumePreparation);
    return () => {
      mounted = false;
      if (refreshTimer) clearInterval(refreshTimer);
      refreshTimer = undefined;
      document.removeEventListener('visibilitychange', resumePreparation);
      cancelHistoryRequests();
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
      if (pinnedToBottom) element.scrollTop = element.scrollHeight;
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

  $effect(() => {
    if (!mounted || snapshotActive || browseState !== 'preparing' || !nextCursor
      || preparationPolls >= maxPreparationPolls || document.visibilityState === 'hidden' || $securityState.locked) return;
    const timer = setTimeout(() => void loadLatest(), preparationIntervalMs);
    return () => clearTimeout(timer);
  });

  // The same per-agent draft store TerminalView uses, so a reply drafted here
  // survives switching views or panes and continues in the terminal composer.
  $effect(() => {
    savePromptDraft(agent, composer);
  });

  function trackScroll() {
    if (!listElement) return;
    // Re-measured on every scroll, so a content shrink — which makes the
    // browser clamp scrollTop and fire a scroll event from a lower position —
    // lands exactly at the bottom and keeps the pin instead of dropping it.
    pinnedToBottom = listElement.scrollHeight
      - listElement.scrollTop
      - listElement.clientHeight < 48;
  }

  function requestWasCancelled(failure: unknown): boolean {
    return typeof failure === 'object' && failure !== null && 'code' in failure && failure.code === 'request_cancelled';
  }

  function cancelHistoryRequests() {
    latestRequestToken++;
    olderRequestToken++;
    loadingOlder = false;
    latestController?.abort();
    olderController?.abort();
    latestController = undefined;
    olderController = undefined;
  }

  function clearHistoryForReload(message: string) {
    cancelHistoryRequests();
    browseState = 'failed';
    loading = false;
    error = message;
    sourceChangedNotice = message;
  }

  function requestLatest() {
    cancelHistoryRequests();
    sourceChangedNotice = '';
    sourceRevision = '';
    snapshotId = '';
    snapshotActive = false;
    snapshotSendNotice = '';
    olderBrowsing = false;
    nextCursor = '';
    hasMore = false;
    total = null;
    omoPlan = null;
    browseState = 'ready';
    browseProgress = undefined;
    preparationPolls = 0;
    entries = [];
    error = '';
    loading = true;
    void loadLatest();
  }

  function reloadHistory() {
    requestLatest();
  }

  function returnToLatest() {
    if (!snapshotActive || loading) return;
    requestLatest();
  }

  function browseTarget(): string {
    return JSON.stringify(agent);
  }

  function historyPageMismatch(page: ConversationPage): string {
    const code = page.error?.code;
    if (['source_changed', 'invalid_cursor', 'cursor_expired'].includes(code || '')) return page.error?.message || '';
    if (sourceRevision && page.sourceRevision && page.sourceRevision !== sourceRevision) return 'The conversation source changed while history was being browsed.';
    if (snapshotId && page.mode === 'snapshot' && page.snapshotId !== snapshotId) return 'The conversation snapshot changed while history was being browsed.';
    return '';
  }

  function continuePreparation() {
    if (!nextCursor || snapshotActive || browseState !== 'preparing' && preparationPolls < maxPreparationPolls) return;
    preparationPolls = 0;
    browseState = 'preparing';
  }

  function cancelPreparation() {
    if (!nextCursor || browseState !== 'preparing') return;
    cancelHistoryRequests();
    preparationPolls = maxPreparationPolls;
    browseState = 'ready';
    browseProgress = undefined;
    error = '';
  }

  function mergeDiagnostics(next: ConversationPage['diagnostics']) {
    if (!next) return;
    diagnostics = {
      oversized_records: Math.max(diagnostics?.oversized_records || 0, next.oversized_records || 0),
      corrupt_records: Math.max(diagnostics?.corrupt_records || 0, next.corrupt_records || 0),
      omitted_tools: Math.max(diagnostics?.omitted_tools || 0, next.omitted_tools || 0),
      omitted_payloads: Math.max(diagnostics?.omitted_payloads || 0, next.omitted_payloads || 0),
      plan_corrupt: Boolean(diagnostics?.plan_corrupt || next.plan_corrupt),
      source_truncated: Boolean(diagnostics?.source_truncated || next.source_truncated),
    };
  }

  async function loadLatest() {
    if (!mounted || (snapshotActive && initialLoadSettled) || sourceChangedNotice || latestController) return;
    const statusCursor = (browseState === 'preparing' || browseState === 'failed') ? nextCursor : '';
    const target = browseTarget();
    const requestToken = ++latestRequestToken;
    const controller = new AbortController();
    latestController = controller;
    if (statusCursor) preparationPolls++;
    try {
      const page = await relayStore.getConversationHistory(agent, {
        ...(statusCursor ? { cursor: statusCursor } : {}),
        signal: controller.signal,
      });
      if (!mounted || requestToken !== latestRequestToken || target !== browseTarget()) return;
      const firstSettledLoad = !initialLoadSettled;
      const wasSnapshot = snapshotActive;
      const wasOlderBrowsing = olderBrowsing;
      const snapshot = page.mode === 'snapshot';
      initialLoadSettled = true;
      const mismatch = historyPageMismatch(page);
      if (mismatch) {
        clearHistoryForReload(mismatch);
        return;
      }
      available = page.available || entries.length > 0;
      reason = page.reason || reason;
      sourceRevision = page.sourceRevision || sourceRevision;
      if (!wasSnapshot || snapshot) total = page.total;
      browseProgress = page.progress;
      browseState = page.state || 'ready';
      if (browseState !== 'preparing') preparationPolls = 0;
      mergeDiagnostics(page.diagnostics);
      if (snapshot) omoPlan = page.omoPlan || omoPlan;
      else if (!wasOlderBrowsing) omoPlan = page.omoPlan || null;
      error = page.error?.message || (browseState === 'failed' ? page.reason : '');
      if (page.available) {
        entries = snapshot ? mergeEntries(page.entries, entries) : mergeEntries(entries, page.entries);
        if (snapshot) {
          snapshotActive = true;
          snapshotId = page.snapshotId || snapshotId;
          nextCursor = page.nextCursor || statusCursor;
          hasMore = page.hasMore || Boolean(nextCursor);
        } else if (!wasOlderBrowsing) {
          nextCursor = page.nextCursor || (browseState === 'failed' ? statusCursor : '');
          hasMore = page.hasMore || Boolean(nextCursor);
        }
      } else if (!entries.length || !statusCursor) {
        nextCursor = '';
        hasMore = false;
      }
      if (firstSettledLoad) onInitialPage?.(page);
    } catch (failure) {
      if (mounted && requestToken === latestRequestToken && !requestWasCancelled(failure)) {
        initialLoadSettled = true;
        browseState = 'failed';
        error = failure instanceof Error ? failure.message : 'Conversation history could not be loaded.';
        if (nextCursor) hasMore = true;
      }
    } finally {
      if (latestController === controller) latestController = undefined;
      if (mounted && requestToken === latestRequestToken) loading = false;
    }
  }

  async function loadOlder() {
    if (!mounted || !nextCursor || loadingOlder) return;
    const requestedCursor = nextCursor;
    const target = browseTarget();
    const requestToken = ++olderRequestToken;
    const controller = new AbortController();
    olderController = controller;
    olderBrowsing = true;
    loadingOlder = true;
    pinnedToBottom = false;
    const previousHeight = listElement?.scrollHeight || 0;
    const previousTop = listElement?.scrollTop || 0;
    const anchor = topVisibleEntry();
    try {
      const page = await relayStore.getConversationHistory(agent, {
        cursor: requestedCursor,
        retry: browseState === 'failed',
        signal: controller.signal,
      });
      if (!mounted || requestToken !== olderRequestToken || target !== browseTarget()) return;
      const mismatch = historyPageMismatch(page);
      if (mismatch) {
        clearHistoryForReload(mismatch);
        return;
      }
      const failed = page.state === 'failed' || page.error || !page.available;
      browseState = page.state || (failed ? 'failed' : 'ready');
      browseProgress = page.progress;
      sourceRevision = page.sourceRevision || sourceRevision;
      mergeDiagnostics(page.diagnostics);
      if (page.omoPlan) omoPlan = page.omoPlan;
      if (page.total !== null) total = page.total;
      error = page.error?.message || (failed ? page.reason : '');
      const retainedCursor = page.nextCursor || requestedCursor;
      nextCursor = failed ? retainedCursor : page.nextCursor || '';
      hasMore = failed ? page.hasMore || Boolean(retainedCursor) : page.hasMore;
      preparationPolls = 0;
      if (page.mode === 'snapshot') {
        snapshotActive = true;
        snapshotId = page.snapshotId || snapshotId;
      }
      if (page.available) entries = mergeEntries(page.entries, entries);
      await tick();
      restoreScrollAnchor(anchor, previousTop, previousHeight);
    } catch (failure) {
      if (mounted && requestToken === olderRequestToken && !requestWasCancelled(failure)) {
        browseState = 'failed';
        error = failure instanceof Error ? failure.message : 'Older turns could not be loaded.';
        nextCursor = requestedCursor;
        hasMore = true;
      }
    } finally {
      if (olderController === controller) olderController = undefined;
      if (mounted && requestToken === olderRequestToken) loadingOlder = false;
    }
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

  function mergeEntries(first: ConversationEntry[], second: ConversationEntry[]): ConversationEntry[] {
    return [...new Map([...first, ...second].map((entry) => [entry.id, entry])).values()];
  }

  function setMode(next: 'conversation' | 'activity') {
    mode = next;
    localStorage.setItem('herdr-conversation-view', next);
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
    const viewingSnapshot = snapshotActive;
    if (!text || inputLocked || sendingPrompt || uploadingAttachment) return;
    sendingPrompt = true;
    composer = '';
    clearPromptDraft(agent);
    try {
      await relayStore.sendToAgent(agent, { type: 'submit_prompt', text });
      relayStore.showToast('Prompt sent.');
      clearUploadStatus();
      if (viewingSnapshot) {
        snapshotSendNotice = 'Prompt sent. Return to latest to view the new reply.';
        return;
      }
      setTimeout(() => {
        if (!mounted) return;
        cancelHistoryRequests();
        snapshotActive = false;
        olderBrowsing = false;
        nextCursor = '';
        hasMore = false;
        omoPlan = null;
        browseState = 'ready';
        browseProgress = undefined;
        preparationPolls = 0;
        void loadLatest();
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
      {#if available && total !== null}<p>{total} recorded {total === 1 ? 'message' : 'messages'}{#if entries.length < total} · {entries.length} loaded{/if}</p>
      {:else if available && entries.length}<p>{entries.length} loaded messages</p>{/if}
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

  {#if loading}
    <div class="empty-state" role="status">Loading conversation…</div>
  {:else if error && !entries.length && browseState !== 'failed'}
    <div class="empty-state" role="alert">{error}</div>
  {:else if !available}
    <div class="empty-state" role="status">{reason || 'Conversation history is unavailable.'}</div>
  {:else}
    {#if sourceChangedNotice}
      <p class="conversation-warning error" role="alert">
        {sourceChangedNotice}
        <Button variant="secondary" size="sm" onclick={reloadHistory}>Reload history</Button>
      </p>
    {/if}
    {#if snapshotActive}
      <p class="conversation-warning" role="status">
        Viewing a stable snapshot of older history. New messages are not included; return to latest to view replies.
        <Button variant="secondary" size="sm" disabled={loading} onclick={returnToLatest}>Return to latest</Button>
      </p>
      {#if snapshotSendNotice}<p class="conversation-warning" role="status">{snapshotSendNotice}</p>{/if}
    {/if}
    {#if hasMore && !sourceChangedNotice && preparationPolls < maxPreparationPolls && browseState !== 'preparing'}
      <div class="conversation-older">
        <Button variant="secondary" size="sm" disabled={loadingOlder || preparationPolls >= maxPreparationPolls} onclick={loadOlder}>
          {loadingOlder ? 'Loading…' : browseState === 'failed' ? 'Retry loading' : 'Load older turns'}
        </Button>
      </div>
    {/if}
    {#if nextCursor && (preparationPolls >= maxPreparationPolls || browseState === 'preparing') && !snapshotActive}
      <p class="conversation-warning" role="status">
        {#if preparationPolls >= maxPreparationPolls}
          Preparation is paused. <Button variant="secondary" size="sm" onclick={continuePreparation}>Continue</Button>
        {:else}
          Preparing history ({browseProgress?.phase || 'scanning'}){#if browseProgress?.source_bytes} — {Math.min(100, Math.round((browseProgress.scanned_bytes / browseProgress.source_bytes) * 100))}% scanned{/if}…
          <Button variant="secondary" size="sm" onclick={cancelPreparation}>Cancel</Button>
        {/if}
      </p>
    {/if}
    {#if diagnostics?.source_truncated && !snapshotActive}
      <p class="conversation-warning" role="status">This log exceeds 16 MB. The newest 16 MB are loaded; older turns remain on this computer and survive relay restarts.</p>
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
    {#if error}<p class="conversation-warning error" role="alert">{error}</p>{/if}
    {#if omoPlan}<OmoPlan plan={omoPlan} />{/if}
    {#if !entries.length}
      <div class="empty-state" role="status">
        {#if hasMore}No turns are in the newest part of this session. Load older turns to browse the rest.
        {:else}No user or assistant turns are recorded for this session.{/if}
      </div>
    {/if}
    {#if entries.length && !modeEntries.length}
      <div class="empty-state" role="status">No user prompts or agent answers are recorded for this session.</div>
    {/if}
    {#if query.trim() && !visibleEntries.length}
      <div class="empty-state" role="status">No loaded turns match “{query.trim()}”.</div>
    {/if}
    <section
      class="conversation-list"
      bind:this={listElement}
      onscroll={trackScroll}
      aria-label={`Conversation with ${agentName}`}
      aria-live="polite"
    >
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

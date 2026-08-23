<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import ConversationMessage from '$components/ConversationMessage.svelte';
  import Button from '$components/ui/Button.svelte';
  import { agentNeedsInspection, agentNeedsResponse, displayName } from '$lib/agents';
  import { conversationEntries } from '$lib/conversation';
  import { clearPromptDraft, loadPromptDraft, savePromptDraft } from '$lib/prompt-drafts';
  import { relayStore } from '$lib/store';
  import type { Agent, ConversationEntry } from '$lib/types';

  let { agent }: { agent: Agent } = $props();

  let entries = $state<ConversationEntry[]>([]);
  let available = $state(true);
  let reason = $state('');
  let hasMore = $state(false);
  let total = $state(0);
  let fileTruncated = $state(false);
  let loading = $state(true);
  let loadingOlder = $state(false);
  let error = $state('');
  let query = $state('');
  let mode = $state<'conversation' | 'activity'>('conversation');
  let listElement = $state<HTMLElement>(null!);
  let streamElement = $state<HTMLElement>(null!);
  let composerElement = $state<HTMLTextAreaElement>(null!);
  let fileInput = $state<HTMLInputElement>(null!);
  let composer = $state(untrack(() => loadPromptDraft(agent)));
  let sendingPrompt = $state(false);
  let uploadingImage = $state(false);
  let uploadStatus = $state('');
  let uploadError = $state(false);
  /**
   * Whether the view follows the end of the transcript. It starts pinned so
   * opening a session lands on the newest turn, and only the reader scrolling
   * away from the bottom releases it.
   */
  let pinnedToBottom = $state(true);
  let mounted = false;

  const modeEntries = $derived(mode === 'conversation' ? conversationEntries(entries) : entries);
  const inputLocked = $derived(agentNeedsResponse(agent) || agentNeedsInspection(agent));
  const inputPlaceholder = $derived(agentNeedsResponse(agent)
    ? 'Needs response — switch to Terminal'
    : agentNeedsInspection(agent)
      ? 'Needs inspection — switch to Terminal'
      : 'Type a reply…');
  const visibleEntries = $derived.by(() => {
    const needle = query.trim().toLocaleLowerCase();
    if (!needle) return modeEntries;
    return modeEntries.filter((entry) => [
      entry.text,
      ...(mode === 'activity' ? (entry.tools || []).flatMap((tool) => [tool.name, tool.input || '', tool.output || '']) : []),
    ].join(' ').toLocaleLowerCase().includes(needle));
  });

  onMount(() => {
    mode = localStorage.getItem('herdr-conversation-view') === 'activity' ? 'activity' : 'conversation';
    mounted = true;
    void loadLatest();
    const refresh = setInterval(() => { void loadLatest(); }, 5_000);
    return () => {
      mounted = false;
      clearInterval(refresh);
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

  async function loadLatest() {
    try {
      const page = await relayStore.getConversationHistory(agent);
      if (!mounted) return;
      available = page.available;
      reason = page.reason;
      hasMore = entries.length ? hasMore : page.hasMore;
      total = page.total;
      fileTruncated = page.fileTruncated;
      error = '';
      if (page.available) entries = mergeEntries(entries, page.entries);
    } catch (failure) {
      if (mounted) error = failure instanceof Error ? failure.message : 'Conversation history could not be loaded.';
    } finally {
      if (mounted) loading = false;
    }
  }

  async function loadOlder() {
    const before = entries[0]?.id || '';
    if (!before || loadingOlder) return;
    loadingOlder = true;
    const previousHeight = listElement?.scrollHeight || 0;
    const previousTop = listElement?.scrollTop || 0;
    try {
      const page = await relayStore.getConversationHistory(agent, before);
      if (!mounted) return;
      hasMore = page.hasMore;
      total = page.total;
      fileTruncated = page.fileTruncated;
      error = '';
      entries = mergeEntries(page.entries, entries);
      await tick();
      if (listElement) listElement.scrollTop = previousTop + listElement.scrollHeight - previousHeight;
    } catch (failure) {
      if (mounted) error = failure instanceof Error ? failure.message : 'Older turns could not be loaded.';
    } finally {
      if (mounted) loadingOlder = false;
    }
  }

  function mergeEntries(first: ConversationEntry[], second: ConversationEntry[]): ConversationEntry[] {
    const merged: ConversationEntry[] = [];
    const indexById = new Map<string, number>();
    for (const entry of [...first, ...second]) {
      const index = indexById.get(entry.id);
      if (index === undefined) {
        indexById.set(entry.id, merged.length);
        merged.push(entry);
      } else {
        merged[index] = entry;
      }
    }
    return merged;
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
    if (!text || inputLocked || sendingPrompt || uploadingImage) return;
    sendingPrompt = true;
    composer = '';
    clearPromptDraft(agent);
    try {
      await relayStore.sendToAgent(agent, { type: 'submit_prompt', text });
      relayStore.showToast('Prompt sent.');
      clearUploadStatus();
      setTimeout(() => { void loadLatest(); }, 500);
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

  async function filesSelected(files: FileList | File[]) {
    const images = [...files].filter((item) => item.type.startsWith('image/'));
    if (!images.length || inputLocked || sendingPrompt || uploadingImage) return;
    uploadingImage = true;
    try {
      for (const file of images) {
        uploadStatus = `Uploading ${file.name || 'image'}…`;
        uploadError = false;
        try {
          const path = await relayStore.uploadImage(agent, file);
          const prefix = composer && !composer.endsWith('\n') ? '\n' : '';
          composer += `${prefix}Image: ${path}\n`;
          uploadStatus = `Image attached: ${path.split(/[\\/]/).pop() || 'image'}`;
        } catch (failure) {
          uploadStatus = failure instanceof Error ? failure.message : 'Image could not be uploaded.';
          uploadError = true;
        }
      }
    } finally {
      uploadingImage = false;
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
</script>

<main class="conversation-page" aria-labelledby="conversation-title">
  <header class="conversation-toolbar">
    <div>
      <h2 id="conversation-title">Conversation</h2>
      {#if available && total}<p>{total} recorded {total === 1 ? 'message' : 'messages'}</p>{/if}
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

  {#if loading}
    <div class="empty-state" role="status">Loading conversation…</div>
  {:else if error && !entries.length}
    <div class="empty-state" role="alert">{error}</div>
  {:else if !available}
    <div class="empty-state" role="status">{reason || 'Conversation history is unavailable.'}</div>
  {:else}
    {#if hasMore}
      <div class="conversation-older">
        <Button variant="secondary" size="sm" disabled={loadingOlder} onclick={loadOlder}>
          {loadingOlder ? 'Loading…' : 'Load older turns'}
        </Button>
      </div>
    {/if}
    {#if fileTruncated}
      <p class="conversation-warning" role="status">This session log is larger than 16 MB. The relay loads its newest 16 MB to bound memory use; older turns remain on this computer and are not removed by a relay restart.</p>
    {/if}
    {#if error}<p class="conversation-warning error" role="alert">{error}</p>{/if}
    {#if !entries.length}
      <div class="empty-state" role="status">No user or assistant turns are recorded for this session.</div>
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
      aria-label={`Conversation with ${displayName(agent)}`}
      aria-live="polite"
    >
      <div class="conversation-stream" bind:this={streamElement}>
        {#each visibleEntries as entry (entry.id)}
          <article class:conversation-user={entry.role === 'user'} class="conversation-entry">
            <header>
              <strong>{entry.role === 'user' ? 'You' : displayName(agent)}</strong>
              <span class="conversation-entry-actions">
                {#if formatTimestamp(entry.timestamp)}<time datetime={entry.timestamp}>{formatTimestamp(entry.timestamp)}</time>{/if}
                {#if entry.text}
                  <Button
                    class="copy-conversation-markdown"
                    variant="ghost"
                    size="icon"
                    aria-label={`Copy ${entry.role === 'user' ? 'your' : displayName(agent)} message as Markdown`}
                    title="Copy Markdown"
                    onclick={() => copyMarkdown(entry)}
                  >
                    <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                      <rect x="9" y="9" width="13" height="13" rx="2"></rect>
                      <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path>
                    </svg>
                  </Button>
                {/if}
              </span>
            </header>
            <ConversationMessage text={entry.text} tools={entry.tools} highlight={query.trim()} />
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
      aria-busy={sendingPrompt || uploadingImage}
      onsubmit={(event) => { event.preventDefault(); void sendPrompt(); }}
    >
      <Button
        variant="ghost"
        size="icon"
        disabled={inputLocked || uploadingImage || sendingPrompt}
        aria-label="Attach image"
        onclick={() => fileInput.click()}
      >
        <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
          <rect x="3" y="4" width="18" height="16" rx="2"></rect>
          <circle cx="8.5" cy="9" r="1.5"></circle>
          <path d="m4 17 4.5-4.5 3.5 3.5 2.5-2.5L20 19"></path>
        </svg>
      </Button>
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
        disabled={!composer.replace(/[\r\n]+$/g, '') || inputLocked || sendingPrompt || uploadingImage}
        aria-label={sendingPrompt ? 'Submitting input' : 'Send prompt'}
      >{sendingPrompt ? '…' : '➤'}</Button>
      <input
        bind:this={fileInput}
        type="file"
        accept="image/*"
        multiple
        hidden
        onchange={(event) => { void filesSelected(event.currentTarget.files || []); event.currentTarget.value = ''; }}
      />
    </form>
    {#if inputLocked}
      <p class="conversation-composer-status" role="status">Switch to Terminal to handle the pending agent interaction.</p>
    {:else if uploadStatus}
      <p class:error={uploadError} class="conversation-composer-status" role="status">{uploadStatus}</p>
    {/if}
  </div>
</main>

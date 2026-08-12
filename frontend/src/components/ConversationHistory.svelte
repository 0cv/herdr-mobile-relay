<script lang="ts">
  import { onMount, tick } from 'svelte';
  import ConversationMessage from '$components/ConversationMessage.svelte';
  import Button from '$components/ui/Button.svelte';
  import { displayName } from '$lib/agents';
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
  let mounted = false;

  const modeEntries = $derived(mode === 'conversation' ? conversationEntries(entries) : entries);
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
    void loadLatest(true);
    const refresh = setInterval(() => { void loadLatest(false); }, 5_000);
    return () => {
      mounted = false;
      clearInterval(refresh);
    };
  });

  async function loadLatest(initial: boolean) {
    const stickToBottom = !listElement || listElement.scrollHeight - listElement.scrollTop - listElement.clientHeight < 48;
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
      if (initial || stickToBottom) {
        await tick();
        if (listElement) listElement.scrollTop = listElement.scrollHeight;
      }
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

  function conversationEntries(recorded: ConversationEntry[]): ConversationEntry[] {
    const conversation: ConversationEntry[] = [];
    let latestAssistant: ConversationEntry | null = null;
    for (const entry of recorded) {
      if (entry.role === 'user') {
        if (latestAssistant) conversation.push(latestAssistant);
        latestAssistant = null;
        conversation.push(entry);
        continue;
      }
      if (entry.text.trim()) latestAssistant = entry;
    }
    if (latestAssistant) conversation.push(latestAssistant);
    return conversation;
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
      relayStore.showToast('Clipboard access is unavailable. Select the message manually.', true);
      return;
    }
    try {
      await navigator.clipboard.writeText(entry.text);
      relayStore.showToast('Markdown copied.');
    } catch {
      relayStore.showToast('Could not copy the Markdown. Select the message manually.', true);
    }
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
    <section class="conversation-list" bind:this={listElement} aria-label={`Conversation with ${displayName(agent)}`} aria-live="polite">
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
          <ConversationMessage text={entry.text} tools={mode === 'activity' ? entry.tools : []} highlight={query.trim()} />
          {#if entry.truncated}<small>Long turn truncated by the relay.</small>{/if}
        </article>
      {/each}
    </section>
  {/if}
</main>

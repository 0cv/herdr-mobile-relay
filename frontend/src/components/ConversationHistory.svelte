<script lang="ts">
  import { onMount, tick } from 'svelte';
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
  let listElement = $state<HTMLElement>(null!);
  let mounted = false;

  const visibleEntries = $derived.by(() => {
    const needle = query.trim().toLocaleLowerCase();
    if (!needle) return entries;
    return entries.filter((entry) => entry.text.toLocaleLowerCase().includes(needle));
  });

  onMount(() => {
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

  function mergeEntries(first: ConversationEntry[], second: ConversationEntry[]): ConversationEntry[] {
    const merged: ConversationEntry[] = [];
    const seen = new Set<string>();
    for (const entry of [...first, ...second]) {
      if (seen.has(entry.id)) continue;
      seen.add(entry.id);
      merged.push(entry);
    }
    return merged;
  }

  function formatTimestamp(value: string): string {
    const timestamp = new Date(value);
    if (Number.isNaN(timestamp.getTime())) return '';
    return timestamp.toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });
  }
</script>

<main class="conversation-page" aria-labelledby="conversation-title">
  <header class="conversation-toolbar">
    <div>
      <h2 id="conversation-title">Conversation</h2>
      {#if available && total}<p>{total} {total === 1 ? 'turn' : 'turns'} in this session</p>{/if}
    </div>
    {#if entries.length}
      <label class="conversation-search">
        <span class="sr-only">Search loaded conversation turns</span>
        <input type="search" bind:value={query} placeholder="Search loaded turns" />
      </label>
    {/if}
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
      <p class="conversation-warning" role="status">The oldest part of this very large session is outside the relay’s bounded read window.</p>
    {/if}
    {#if error}<p class="conversation-warning error" role="alert">{error}</p>{/if}
    {#if !entries.length}
      <div class="empty-state" role="status">No user or assistant turns are recorded for this session.</div>
    {/if}
    {#if query.trim() && !visibleEntries.length}
      <div class="empty-state" role="status">No loaded turns match “{query.trim()}”.</div>
    {/if}
    <section class="conversation-list" bind:this={listElement} aria-label={`Conversation with ${displayName(agent)}`} aria-live="polite">
      {#each visibleEntries as entry (entry.id)}
        <article class:conversation-user={entry.role === 'user'} class="conversation-entry">
          <header>
            <strong>{entry.role === 'user' ? 'You' : displayName(agent)}</strong>
            {#if formatTimestamp(entry.timestamp)}<time datetime={entry.timestamp}>{formatTimestamp(entry.timestamp)}</time>{/if}
          </header>
          <div class="conversation-text">{entry.text}</div>
          {#if entry.truncated}<small>Long turn truncated by the relay.</small>{/if}
        </article>
      {/each}
    </section>
  {/if}
</main>

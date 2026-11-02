<script lang="ts">
  import Button from '$components/ui/Button.svelte';
  import type { RelayConnectionView } from '$lib/types';

  let {
    connection,
    cwd,
    listId,
    open = $bindable(false),
    onselect,
    notice = '',
    emptyNotice = '',
  }: {
    connection: Pick<RelayConnectionView, 'directoryBrowser' | 'directoryLoading' | 'directoryError'> | undefined;
    cwd: string;
    listId: string;
    open?: boolean;
    onselect: (path: string) => void;
    notice?: string;
    emptyNotice?: string;
  } = $props();
</script>

<div class="directory-toolbar">
  <Button
    size="icon"
    variant="secondary"
    aria-label="Open parent directory"
    disabled={!connection?.directoryBrowser?.parent}
    onclick={() => connection?.directoryBrowser?.parent && onselect(connection.directoryBrowser.parent)}
  >↑</Button>
  <button
    class="directory-current"
    type="button"
    aria-expanded={open}
    aria-controls={listId}
    onclick={() => { open = !open; }}
  >
    <span>{connection?.directoryBrowser?.current.label || cwd || (connection?.directoryLoading ? 'Loading…' : 'Unavailable')}</span>
    <span aria-hidden="true">⌄</span>
  </button>
</div>
{#if open}
  <div id={listId} class="directory-list" aria-label="Subdirectories">
    {#if notice}
      <p>{notice}</p>
    {:else if connection?.directoryLoading}
      <p>Loading folders…</p>
    {:else if connection?.directoryError}
      <p role="alert">{connection.directoryError}</p>
    {:else}
      {#if connection?.directoryBrowser?.parent}
        <button type="button" onclick={() => onselect(connection.directoryBrowser?.parent || '')}>↰ Parent folder</button>
      {/if}
      {#each connection?.directoryBrowser?.directories || [] as directory (directory.path)}
        <button type="button" onclick={() => onselect(directory.path)}>📁 {directory.name}</button>
      {/each}
      {#if emptyNotice && connection?.directoryBrowser && !connection.directoryBrowser.directories.length}
        <p>{emptyNotice}</p>
      {/if}
    {/if}
  </div>
{/if}

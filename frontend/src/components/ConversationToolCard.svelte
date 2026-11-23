<script lang="ts">
  import ToolPayload from '$components/ToolPayload.svelte';
  import type { ConversationMessageViewState } from '$lib/conversation-message-state';
  import type { ConversationEditDiff, ConversationTool } from '$lib/types';

  let { tool, diff, viewState, cardKey }: {
    tool: ConversationTool;
    diff?: ConversationEditDiff;
    viewState: ConversationMessageViewState;
    cardKey: string;
  } = $props();

  let open = $state(false);
  const label = $derived(diff?.kind === 'write' ? 'Written content' : 'Requested change');
  const fileName = $derived(diff?.path.split(/[\\/]/u).pop() || diff?.path);

  function preserveToolState(node: HTMLDetailsElement, initial: { state: ConversationMessageViewState; key: string }) {
    let current = initial;
    const restore = () => { open = node.open = current.state.openTools.has(current.key); };
    restore();
    const save = () => {
      open = node.open;
      if (node.open && current.state.openTools.size < 64) current.state.openTools.add(current.key);
      else if (!node.open) current.state.openTools.delete(current.key);
    };
    node.addEventListener('toggle', save);
    return {
      update(next: typeof initial) {
        if (next.state !== current.state || next.key !== current.key) { current = next; restore(); }
      },
      destroy() { save(); node.removeEventListener('toggle', save); },
    };
  }

  function rowTone(row: string): string {
    return row[0] === '+' ? 'diff-addition' : row[0] === '-' ? 'diff-deletion' : 'diff-hunk';
  }
</script>

<details class:error={tool.error} use:preserveToolState={{ state: viewState, key: cardKey }}>
  <summary>
    <span aria-hidden="true">{tool.error ? '!' : '›'}</span>
    <strong>{tool.name}</strong>
    {#if diff}
      <small class="edit-filename">{fileName}</small>
      <small class="edit-counts" aria-hidden="true">+{diff.added} / -{diff.removed}</small>
      <span class="sr-only">{diff.added} {diff.added === 1 ? 'line' : 'lines'} added, {diff.removed} {diff.removed === 1 ? 'line' : 'lines'} removed</span>
    {/if}
    <small>{tool.state || (tool.error ? 'failed' : tool.output ? 'completed' : 'called')}</small>
    {#if tool.parentId}<small>nested tool</small>{/if}
  </summary>
  <div class="conversation-tool-detail">
    {#if diff}
      {#if open}
        <section>
          <p class="edit-path">{diff.path}</p>
          <h4>{label}</h4>
          <pre aria-label={label}>{#if diff.text}{#each diff.text.split('\n') as row, index (index)}<span class={`workspace-diff-line ${rowTone(row)}`}>{row}</span>{/each}{/if}</pre>
          {#if diff.truncated}<p>Diff truncated</p>{/if}
        </section>
      {/if}
    {:else if tool.input}
      <ToolPayload label="Input" raw={tool.input} />
    {/if}
    {#if tool.output}
      <ToolPayload label="Output" raw={tool.output} />
    {:else}
      <p>{tool.state === 'running' ? 'Waiting for tool output…' : 'No output was captured in this session log.'}</p>
    {/if}
    {#if tool.truncated}<p>Tool detail was truncated by the relay.</p>{/if}
  </div>
</details>

<style>
  .edit-filename { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .edit-counts { white-space: nowrap; }
  .edit-path { overflow-wrap: anywhere; margin: 0 0 .4rem; white-space: pre-wrap; }
</style>

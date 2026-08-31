<script module lang="ts">
  const tableOffsets = new Map<string, number[]>();
</script>

<script lang="ts">
  import ToolPayload from '$components/ToolPayload.svelte';
  import { safeMarkdownHtml } from '$lib/markdown';
  import type { ConversationTool } from '$lib/types';
  function preserveTableScroll(node: HTMLElement, identity: string) {
    let key = identity;
    let cleanups: Array<() => void> = [];
    const bind = () => {
      for (const cleanup of cleanups) cleanup();
      cleanups = [];
      const tables = [...node.querySelectorAll<HTMLElement>('.conversation-table')];
      const offsets = tableOffsets.get(key) || [];
      for (let index = 0; index < tables.length; index += 1) {
        const table = tables[index];
        table.scrollLeft = offsets[index] || 0;
        const save = () => {
          const next = tableOffsets.get(key)?.slice() || [];
          next[index] = table.scrollLeft;
          tableOffsets.set(key, next);
        };
        table.addEventListener('scroll', save, { passive: true });
        cleanups.push(() => table.removeEventListener('scroll', save));
      }
    };
    const observer = new MutationObserver(bind);
    observer.observe(node, { childList: true, subtree: true });
    bind();
    return {
      update(next: string) {
        if (next === key) return;
        tableOffsets.delete(key);
        key = next;
        bind();
      },
      destroy() {
        observer.disconnect();
        for (const cleanup of cleanups) cleanup();
        tableOffsets.delete(key);
      },
    };
  }


  let {
    text,
    messageId,
    tools = [],
    highlight = '',
  }: {
    text: string;
    messageId: string;
    tools?: ConversationTool[];
    highlight?: string;
  } = $props();

  const markdown = $derived(safeMarkdownHtml(text, highlight));
</script>

{#if text}
  <div class="conversation-markdown" use:preserveTableScroll={messageId}>{@html markdown}</div>
{/if}

{#if tools.length}
  <div class="conversation-tools" aria-label="Tool activity">
    {#each tools as tool, index (`${tool.id || tool.name}:${index}`)}
      <details class:error={tool.error}>
        <summary>
          <span aria-hidden="true">{tool.error ? '!' : '›'}</span>
          <strong>{tool.name}</strong>
          <small>{tool.error ? 'failed' : tool.output ? 'completed' : 'called'}</small>
        </summary>
        <div class="conversation-tool-detail">
          {#if tool.input}
            <ToolPayload label="Input" raw={tool.input} />
          {/if}
          {#if tool.output}
            <ToolPayload label="Output" raw={tool.output} />
          {:else}
            <p>No output was captured in this session log.</p>
          {/if}
          {#if tool.truncated}<p>Tool detail was truncated by the relay.</p>{/if}
        </div>
      </details>
    {/each}
  </div>
{/if}

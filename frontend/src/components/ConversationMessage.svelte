<script lang="ts">
  import ToolPayload from '$components/ToolPayload.svelte';
  import { safeMarkdownHtml } from '$lib/markdown';
  import type { ConversationTool } from '$lib/types';

  let {
    text,
    tools = [],
    highlight = '',
  }: {
    text: string;
    tools?: ConversationTool[];
    highlight?: string;
  } = $props();

  const markdown = $derived(safeMarkdownHtml(text, highlight));
</script>

{#if text}
  <div class="conversation-markdown">{@html markdown}</div>
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

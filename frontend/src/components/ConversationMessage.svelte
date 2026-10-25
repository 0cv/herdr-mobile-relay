<script lang="ts">
  import ToolPayload from '$components/ToolPayload.svelte';
  import { parseAskUserQuestion } from '$lib/conversation';
  import { safeMarkdownHtml } from '$lib/markdown';
  import type { ConversationTool } from '$lib/types';
  import type { ConversationMessageViewState } from '$lib/conversation-message-state';

  function preserveTableScroll(node: HTMLElement, initial: ConversationMessageViewState) {
    let state = initial;
    let cleanups: Array<() => void> = [];
    const bind = () => {
      for (const cleanup of cleanups) cleanup();
      cleanups = [];
      const tables = [...node.querySelectorAll<HTMLElement>('.conversation-table')];
      for (let index = 0; index < tables.length; index += 1) {
        const table = tables[index];
        table.scrollLeft = state.tableOffsets[index] || 0;
        const save = () => { if (index < 16) state.tableOffsets[index] = Math.max(0, table.scrollLeft); };
        table.addEventListener('scroll', save, { passive: true });
        cleanups.push(() => table.removeEventListener('scroll', save));
      }
    };
    const observer = new MutationObserver(bind);
    observer.observe(node, { childList: true, subtree: true });
    bind();
    return {
      update(next: ConversationMessageViewState) {
        if (next === state) return;
        state = next;
        bind();
      },
      destroy() {
        observer.disconnect();
        for (const cleanup of cleanups) cleanup();
      },
    };
  }

  function preserveToolState(node: HTMLDetailsElement, initial: { state: ConversationMessageViewState; key: string }) {
    let current = initial;
    node.open = current.state.openTools.has(current.key);
    const save = () => {
      if (node.open && current.state.openTools.size < 64) current.state.openTools.add(current.key);
      else if (!node.open) current.state.openTools.delete(current.key);
    };
    node.addEventListener('toggle', save);
    return {
      update(next: typeof initial) {
        if (next.state !== current.state || next.key !== current.key) { current = next; node.open = current.state.openTools.has(current.key); }
      },
      destroy() { save(); node.removeEventListener('toggle', save); },
    };
  }

  let {
    text,
    viewState,
    tools = [],
    highlight = '',
  }: {
    text: string;
    viewState: ConversationMessageViewState;
    tools?: ConversationTool[];
    highlight?: string;
  } = $props();

  const markdown = $derived(safeMarkdownHtml(text, highlight));
</script>

{#if text}
  <div class="conversation-markdown" use:preserveTableScroll={viewState}>{@html markdown}</div>
{/if}

{#if tools.length}
  <div class="conversation-tools" aria-label="Tool activity">
    {#each tools as tool, index (`${tool.id || tool.name}:${index}`)}
      {@const asked = tool.name === 'AskUserQuestion' && tool.input ? parseAskUserQuestion(tool.input, tool.error ? '' : tool.output) : null}
      <details class:error={tool.error} use:preserveToolState={{ state: viewState, key: `${tool.id || tool.name}:${index}` }}>
        <summary>
          <span aria-hidden="true">{tool.error ? '!' : '›'}</span>
          <strong>{tool.name}</strong>
          <small>{tool.state || (tool.error ? 'failed' : tool.output ? 'completed' : 'called')}</small>
          {#if tool.parentId}<small>nested tool</small>{/if}
        </summary>
        <div class="conversation-tool-detail">
          {#if asked}
            {#each asked as item, itemIndex (itemIndex)}
              <section class="conversation-question">
                {#if item.header}<h4>{item.header}{item.multiSelect ? ' · multiple choice' : ''}</h4>{/if}
                <p><strong>{item.question}</strong></p>
                <ul>
                  {#each item.options as option, optionIndex (optionIndex)}
                    <li class:chosen={item.answer === option.label}><span>{option.label}</span>{#if option.description}<small>{option.description}</small>{/if}</li>
                  {/each}
                </ul>
                {#if item.answer !== undefined}<p class="conversation-answer">Answered: {item.answer}</p>{/if}
              </section>
            {/each}
          {:else if tool.input}
            <ToolPayload label="Input" raw={tool.input} />
          {/if}
          {#if asked && asked.every((item) => item.answer !== undefined)}
            <!-- answers already shown beside their options -->
          {:else if tool.output}
            <ToolPayload label="Output" raw={tool.output} />
          {:else}
            <p>{tool.state === 'running' ? 'Waiting for tool output…' : 'No output was captured in this session log.'}</p>
          {/if}
          {#if tool.truncated}<p>Tool detail was truncated by the relay.</p>{/if}
        </div>
      </details>
    {/each}
  </div>
{/if}

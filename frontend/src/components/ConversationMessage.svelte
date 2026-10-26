<script lang="ts">
  import ToolPayload from '$components/ToolPayload.svelte';
  import { isChosenOption, parseAskUserQuestion } from '$lib/conversation';
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
    awaiting = null,
  }: {
    text: string;
    viewState: ConversationMessageViewState;
    tools?: ConversationTool[];
    highlight?: string;
    awaiting?: 'below' | 'elsewhere' | null;
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
      {#if asked}
        {@const waiting = !tool.output && !tool.error}
        <section class="conversation-ask" class:error={tool.error} aria-label="Question from the agent">
          {#each asked as item, itemIndex (itemIndex)}
            <section class="conversation-question">
              {#if item.header || item.multiSelect}<h4>{item.header ? `${item.header}${item.multiSelect ? ' · multiple choice' : ''}` : 'Multiple choice'}</h4>{/if}
              <p><strong>{item.question}</strong></p>
              {#if item.options.length && !(waiting && awaiting === 'below')}
                <ul role="list">
                  {#each item.options as option, optionIndex (optionIndex)}
                    {@const chosen = isChosenOption(item, option.label)}
                    <li class:chosen><span>{option.label}</span>{#if chosen}<span class="sr-only"> (selected)</span>{/if}{#if option.description}<small>{option.description}</small>{/if}</li>
                  {/each}
                </ul>
              {/if}
              {#if item.answer !== undefined}<p class="conversation-answer">Answered: {item.answer}</p>{/if}
            </section>
          {/each}
          {#if asked.every((item) => item.answer !== undefined)}
            <!-- answers already shown beside their options -->
          {:else if tool.output}
            <ToolPayload label={tool.error ? 'Error' : 'Reply'} raw={tool.output} />
          {:else if waiting && awaiting === 'below'}
            <p role="status">Waiting for your answer below.</p>
          {:else if waiting && awaiting === 'elsewhere'}
            <p role="status">Waiting for an answer.</p>
          {:else}
            <p>No answer was recorded in this session log.</p>
          {/if}
          {#if tool.truncated}<p>Tool detail was truncated by the relay.</p>{/if}
        </section>
      {:else}
        <details class:error={tool.error} use:preserveToolState={{ state: viewState, key: `${tool.id || tool.name}:${index}` }}>
          <summary>
            <span aria-hidden="true">{tool.error ? '!' : '›'}</span>
            <strong>{tool.name}</strong>
            <small>{tool.state || (tool.error ? 'failed' : tool.output ? 'completed' : 'called')}</small>
            {#if tool.parentId}<small>nested tool</small>{/if}
          </summary>
          <div class="conversation-tool-detail">
            {#if tool.input}
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
      {/if}
    {/each}
  </div>
{/if}

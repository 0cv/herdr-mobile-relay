<script lang="ts">
  import ToolPayload from '$components/ToolPayload.svelte';
  import ConversationToolCard from '$components/ConversationToolCard.svelte';
  import { isChosenOption, parseAskUserQuestion } from '$lib/conversation';
  import type { FileLinkContext } from '$lib/file-links';
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

  let {
    text,
    role,
    viewState,
    tools = [],
    showDiffs = true,
    highlight = '',
    fileLinks,
    awaiting = null,
  }: {
    text: string;
    role?: string;
    viewState: ConversationMessageViewState;
    tools?: ConversationTool[];
    showDiffs?: boolean;
    highlight?: string;
    fileLinks?: FileLinkContext | null;
    awaiting?: 'below' | 'elsewhere' | null;
  } = $props();

  const markdown = $derived(safeMarkdownHtml(text, highlight, fileLinks, role));
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
        <ConversationToolCard {tool} diff={showDiffs ? tool.diff : undefined} {viewState} cardKey={`${tool.id || tool.name}:${index}`} />
      {/if}
    {/each}
  </div>
{/if}

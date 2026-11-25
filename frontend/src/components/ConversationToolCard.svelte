<script lang="ts">
  import CodeTokens from '$components/CodeTokens.svelte';
  import { fileLanguage, fitsSyntaxBudget, loadCodeColours, type LoadSyntax, type ScanCode } from '$lib/code-reading';
  import ToolPayload from '$components/ToolPayload.svelte';
  import type { ConversationMessageViewState } from '$lib/conversation-message-state';
  import type { ConversationEditDiff, ConversationTool } from '$lib/types';

  let { tool, diff, viewState, cardKey, provisional, loadSyntax }: {
    tool: ConversationTool;
    diff?: ConversationEditDiff;
    viewState: ConversationMessageViewState;
    cardKey: string;
    provisional: boolean;
    loadSyntax?: LoadSyntax;
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

  let scanner = $state<ScanCode | null>(null);
  let preview = $state<HTMLElement>();
  const language = $derived(open && !provisional && tool.state !== 'running' && diff?.text
    && !diff.truncated && fitsSyntaxBudget(diff.text) ? fileLanguage(diff.path) : undefined);
  $effect(() => {
    if (!language || !preview || !loadSyntax || scanner) return;
    return loadCodeColours(preview, loadSyntax, scan => { scanner = scan; });
  });
  const rows = $derived(open && diff?.text ? diff.text.split('\n').map(row => {
    const sign = row[0];
    const tone = sign === '+' ? 'diff-addition' : sign === '-' ? 'diff-deletion' : 'diff-hunk';
    const tokens = language && scanner && (sign === '+' || sign === '-') ? scanner(row.slice(1), language) : null;
    return { row, sign, tone, tokens };
  }) : []);
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
          <pre aria-label={label} bind:this={preview}>{#each rows as { row, sign, tone, tokens }, index (index)}<span class={`workspace-diff-line ${tone}`}>{#if tokens}{sign}<CodeTokens {tokens} />{:else}{row}{/if}</span>{/each}</pre>
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

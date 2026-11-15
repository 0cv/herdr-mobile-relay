<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import {
    findTerminalText,
    terminalMatchFragments,
    terminalRowForOffset,
    terminalRowOffsets,
    terminalSearchText,
  } from '$lib/terminal-find';
  import type { RenderedTerminalRow } from '$lib/terminal';

  let {
    rows,
    terminal,
    mountedRange,
    domVersion,
    disabled,
    scrollToRow,
    open: findOpen = $bindable(false),
  }: {
    rows: RenderedTerminalRow[];
    terminal: HTMLElement;
    mountedRange: () => { start: number; end: number };
    domVersion: string;
    disabled: boolean;
    scrollToRow: (row: number) => Promise<void>;
    open?: boolean;
  } = $props();

  let findInputElement = $state<HTMLInputElement>(null!);
  let findQuery = $state('');
  let activeFindIndex = $state(-1);

  const terminalFindCorpus = $derived.by(() => ({
    text: terminalSearchText(rows),
    offsets: terminalRowOffsets(rows),
  }));
  const terminalFind = $derived(findTerminalText(terminalFindCorpus.text, findQuery.trim()));

  $effect(() => {
    const count = terminalFind.matches.length;
    const query = findQuery.trim();
    untrack(() => {
      if (!query || !count) {
        activeFindIndex = -1;
        return;
      }
      if (activeFindIndex < 0 || activeFindIndex >= count) activeFindIndex = 0;
    });
  });

  $effect(() => {
    const highlightState = [
      findOpen,
      findQuery,
      activeFindIndex,
      domVersion,
      terminalFind.matches.length,
    ];
    void highlightState;
    void tick().then(applyTerminalFindHighlights);
  });

  onMount(() => {
    const findShortcut = (event: KeyboardEvent) => {
      if (disabled
        || event.altKey
        || (!event.ctrlKey && !event.metaKey)
        || event.key.toLocaleLowerCase() !== 'f') return;
      event.preventDefault();
      openTerminalFind();
    };
    window.addEventListener('keydown', findShortcut);
    return () => window.removeEventListener('keydown', findShortcut);
  });

  function clearTerminalFindHighlights() {
    if (!terminal) return;
    const parents = new Set<Node>();
    for (const mark of terminal.querySelectorAll<HTMLElement>('mark[data-terminal-find]')) {
      if (mark.parentNode) parents.add(mark.parentNode);
      mark.replaceWith(document.createTextNode(mark.textContent || ''));
    }
    for (const parent of parents) parent.normalize();
  }

  function applyTerminalFindHighlights() {
    clearTerminalFindHighlights();
    if (!terminal || !findOpen || !findQuery.trim() || !terminalFind.matches.length) return;
    const { start, end } = mountedRange();
    const byRow = new Map<number, Array<{ start: number; end: number; active: boolean }>>();
    terminalFind.matches.forEach((match, matchIndex) => {
      for (const fragment of terminalMatchFragments(rows, terminalFindCorpus.offsets, match)) {
        if (fragment.row < start || fragment.row >= end) continue;
        const fragments = byRow.get(fragment.row) || [];
        fragments.push({ start: fragment.start, end: fragment.end, active: matchIndex === activeFindIndex });
        byRow.set(fragment.row, fragments);
      }
    });
    for (const [row, fragments] of byRow) {
      const element = terminal.querySelector<HTMLElement>(`[data-terminal-row="${row}"]`);
      if (element) highlightTerminalRow(element, fragments);
    }
  }

  function highlightTerminalRow(
    element: HTMLElement,
    fragments: Array<{ start: number; end: number; active: boolean }>,
  ) {
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    const nodes: Array<{ node: Text; start: number; end: number }> = [];
    let offset = 0;
    let node = walker.nextNode();
    while (node) {
      const text = node as Text;
      nodes.push({ node: text, start: offset, end: offset + text.data.length });
      offset += text.data.length;
      node = walker.nextNode();
    }
    for (const entry of nodes) {
      const intersections = fragments
        .map((fragment) => ({
          start: Math.max(fragment.start, entry.start) - entry.start,
          end: Math.min(fragment.end, entry.end) - entry.start,
          active: fragment.active,
        }))
        .filter((fragment) => fragment.end > fragment.start)
        .sort((left, right) => right.start - left.start);
      for (const fragment of intersections) {
        entry.node.splitText(fragment.end);
        const selected = entry.node.splitText(fragment.start);
        const mark = document.createElement('mark');
        mark.dataset.terminalFind = '';
        mark.className = `terminal-find-match${fragment.active ? ' active' : ''}`;
        selected.replaceWith(mark);
        mark.append(selected);
      }
    }
  }

  function openTerminalFind() {
    findOpen = true;
    void tick().then(() => {
      findInputElement?.focus();
      findInputElement?.select();
    });
  }

  export function open() {
    openTerminalFind();
  }

  function closeTerminalFind() {
    findOpen = false;
  }

  function findInputChanged() {
    activeFindIndex = -1;
    void tick().then(() => {
      if (terminalFind.matches.length) revealFindMatch(0);
    });
  }

  function findKeydown(event: KeyboardEvent) {
    if (event.key === 'Escape') {
      event.preventDefault();
      closeTerminalFind();
      return;
    }
    if (event.key !== 'Enter') return;
    event.preventDefault();
    revealFindMatch(activeFindIndex + (event.shiftKey ? -1 : 1));
  }

  async function revealFindMatch(index: number) {
    const count = terminalFind.matches.length;
    if (!terminal || !count) return;
    const normalized = ((index % count) + count) % count;
    activeFindIndex = normalized;
    const match = terminalFind.matches[normalized];
    const row = terminalRowForOffset(rows, terminalFindCorpus.offsets, match.start);
    if (row < 0) return;
    await scrollToRow(row);
    applyTerminalFindHighlights();
  }
</script>

{#snippet findPreviousIcon()}
  <svg class="find-action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="m6 15 6-6 6 6"></path>
  </svg>
{/snippet}

{#snippet findNextIcon()}
  <svg class="find-action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="m6 9 6 6 6-6"></path>
  </svg>
{/snippet}

{#snippet findCloseIcon()}
  <svg class="find-action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="m6 6 12 12M18 6 6 18"></path>
  </svg>
{/snippet}

{#if findOpen}
  <section class="terminal-find" aria-label="Find in terminal">
    <input
      bind:this={findInputElement}
      bind:value={findQuery}
      type="search"
      aria-label="Find in terminal output"
      placeholder="Find in terminal"
      autocomplete="off"
      autocapitalize="none"
      spellcheck="false"
      oninput={findInputChanged}
      onkeydown={findKeydown}
    />
    {#if findQuery.trim()}
      <span class="terminal-find-count" role="status" aria-live="polite">
        {#if !terminalFind.matches.length}
          No matches
        {:else}
          {activeFindIndex + 1} of {terminalFind.matches.length}{terminalFind.truncated ? '+' : ''}
        {/if}
      </span>
    {/if}
    <Button variant="secondary" size="sm" aria-label="Previous match" disabled={!terminalFind.matches.length} onclick={() => revealFindMatch(activeFindIndex - 1)}>
      {@render findPreviousIcon()}
    </Button>
    <Button variant="secondary" size="sm" aria-label="Next match" disabled={!terminalFind.matches.length} onclick={() => revealFindMatch(activeFindIndex + 1)}>
      {@render findNextIcon()}
    </Button>
    <Button variant="ghost" size="sm" aria-label="Close find" onclick={closeTerminalFind}>
      {@render findCloseIcon()}
    </Button>
  </section>
{/if}

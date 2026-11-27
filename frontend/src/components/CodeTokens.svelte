<script lang="ts">
  import type { CodeToken } from '$lib/code-reading';

  let { tokens }: { tokens: CodeToken[] } = $props();
</script>

{#each tokens as { text, kind }, index (index)}{#if kind}<span class={`syntax-${kind}`}>{text}</span>{:else}{text}{/if}{/each}

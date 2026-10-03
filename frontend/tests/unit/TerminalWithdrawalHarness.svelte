<script lang="ts">
  import TerminalView from '$components/TerminalView.svelte';
  import type { Agent } from '$lib/types';
  let { agent }: { agent: Agent | null } = $props();
</script>

{#if agent}
  {#key agent.pane_id}
    <TerminalView {agent} allAgents={[agent]} responding={new Set<string>()} />
  {/key}
{/if}

<script lang="ts">
  import { onMount } from 'svelte';
  import AgentLogo from '$components/AgentLogo.svelte';
  import { agentStatusTone, displayName, hostLabel, tabName } from '$lib/agents';
  import { resumeMetrics, shownRelayRows } from '$lib/resume-metrics';
  import type { Agent } from '$lib/types';
  import { workspaceGroups } from '$lib/workspaces';

  let {
    agents,
    active,
    onopen,
    onjump,
  }: {
    agents: Agent[];
    active: Agent;
    onopen: (agent: Agent) => void;
    onjump: () => void;
  } = $props();

  const groups = $derived(workspaceGroups(agents));
  let railRoot = $state<HTMLElement>();
  // Beside a terminal the rail is the visible agent inventory, but only on
  // wide screens (app.css hides it below 900px). It counts for local resume
  // timing only while shown, and is checked again at the frame itself.
  onMount(() => {
    const wide = typeof window.matchMedia === 'function' ? window.matchMedia('(min-width: 900px)') : null;
    let hide: (() => void) | null = null;
    const sync = () => {
      hide?.();
      hide = !wide || wide.matches ? resumeMetrics.presentInventory((relayId) => shownRelayRows(railRoot, relayId)) : null;
    };
    sync();
    wide?.addEventListener('change', sync);
    return () => {
      wide?.removeEventListener('change', sync);
      hide?.();
    };
  });
</script>

<aside bind:this={railRoot} class="agent-rail" aria-label="Agent navigation">
  <header>
    <strong>Agents</strong>
    <button type="button" onclick={onjump} aria-label="Search all agents" title="Search all agents">⌕</button>
  </header>
  <div class="agent-rail-groups">
    {#each groups as group (group.key)}
      <section aria-label={`${group.label} workspace on ${group.host}`}>
        <h2 title={group.cwd}>{group.label}<small>@{group.host}</small></h2>
        {#each group.agents as agent (agent.pane_id)}
          <button data-live-relay={agent.relay_id} class:active={agent.pane_id === active.pane_id} type="button" aria-current={agent.pane_id === active.pane_id ? 'page' : undefined} onclick={() => onopen(agent)}>
            <span class="agent-identity">
              <AgentLogo agent={agent.agent} />
              <span class={`status-dot status-${agentStatusTone(agent)}`} aria-hidden="true"></span>
            </span>
            <span>
              <strong>{displayName(agent)}</strong>
              <small>{[tabName(agent), agent.session, hostLabel(agent) !== group.host ? `@${hostLabel(agent)}` : ''].filter(Boolean).join(' · ')}</small>
            </span>
          </button>
        {/each}
      </section>
    {/each}
  </div>
</aside>

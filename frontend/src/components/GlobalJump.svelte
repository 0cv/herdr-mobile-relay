<script lang="ts">
  import { tick } from 'svelte';
  import AgentLogo from '$components/AgentLogo.svelte';
  import AppDialog from '$components/ui/AppDialog.svelte';
  import { agentStatusTone, displayName, hostLabel, tabName } from '$lib/agents';
  import type { Agent } from '$lib/types';
  import {
    agentSearchText,
    workspaceGroups,
    workspaceMetadataSearchText,
    type WorkspaceGroup,
  } from '$lib/workspaces';

  let {
    open = $bindable(false),
    agents,
    onselect,
  }: {
    open?: boolean;
    agents: Agent[];
    onselect: (agent: Agent) => void;
  } = $props();

  let query = $state('');
  let input = $state<HTMLInputElement>();
  const groups = $derived(workspaceGroups(agents));
  const needle = $derived(query.trim().toLocaleLowerCase());
  function visibleAgents(group: WorkspaceGroup): Agent[] {
    if (!needle || workspaceMetadataSearchText(group).includes(needle)) return group.agents;
    return group.agents.filter((agent) => agentSearchText(agent).includes(needle));
  }
  const matchingGroups = $derived(groups.filter((group) => visibleAgents(group).length > 0));

  $effect(() => {
    if (!open) {
      query = '';
      return;
    }
    void tick().then(() => input?.focus());
  });

  function choose(agent: Agent) {
    open = false;
    onselect(agent);
  }
</script>

<AppDialog id="global-jump-dialog" bind:open title="Jump to agent" description="Search workspaces, projects, tabs, agents, sessions, paths, relays, and hosts.">
  <label class="jump-search">
    <span class="sr-only">Search agents and workspaces</span>
    <input bind:this={input} bind:value={query} type="search" placeholder="Search all agents…" autocomplete="off" />
  </label>
  <div class="jump-results" aria-live="polite">
    {#if needle && !matchingGroups.length}
      <div class="empty-state">No matching agents.</div>
    {:else}
      {#each matchingGroups as group (group.key)}
        {@const visible = visibleAgents(group)}
        {#if visible.length}
          <section aria-label={`${group.label} workspace on ${group.host}`}>
            <header>
              <strong>{group.label}</strong>
              <span>{group.cwd || `@${group.host}`}</span>
            </header>
            {#each visible as agent (agent.pane_id)}
              <button type="button" onclick={() => choose(agent)}>
                <span class="agent-identity">
                  <AgentLogo agent={agent.agent} />
                  <span class={`status-dot status-${agentStatusTone(agent)}`} aria-hidden="true"></span>
                </span>
                <span>
                  <strong>{displayName(agent)}</strong>
                  <small>{[tabName(agent), agent.session, `@${hostLabel(agent)}`].filter(Boolean).join(' · ')}</small>
                </span>
              </button>
            {/each}
          </section>
        {/if}
      {/each}
    {/if}
  </div>
</AppDialog>

<script lang="ts">
  import AgentLogo from '$components/AgentLogo.svelte';
  import { agentStatusTone, displayName, hostLabel, tabName } from '$lib/agents';
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
</script>

<aside class="agent-rail" aria-label="Agent navigation">
  <header>
    <strong>Agents</strong>
    <button type="button" onclick={onjump} aria-label="Search all agents" title="Search all agents">⌕</button>
  </header>
  <div class="agent-rail-groups">
    {#each groups as group (group.key)}
      <section aria-label={`${group.label} workspace on ${group.host}`}>
        <h2 title={group.cwd}>{group.label}<small>@{group.host}</small></h2>
        {#each group.agents as agent (agent.pane_id)}
          <button class:active={agent.pane_id === active.pane_id} type="button" aria-current={agent.pane_id === active.pane_id ? 'page' : undefined} onclick={() => onopen(agent)}>
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

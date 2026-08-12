<script lang="ts">
  import { onMount } from 'svelte';
  import AgentLogo, { hasAgentLogo } from '$components/AgentLogo.svelte';
  import Button from '$components/ui/Button.svelte';
  import {
    agentLastActiveAt,
    agentStatusGroup,
    agentStatusTone,
    approvalButtonTone,
    approvalOptions,
    approvalPromptPreview,
    displayName,
    hostLabel,
    questionInteraction,
    sortedAgents,
    tabName,
  } from '$lib/agents';
  import { relayStore } from '$lib/store';
  import type { Agent, RelayConfig, RelayConnectionView } from '$lib/types';
  import { workspaceGroups } from '$lib/workspaces';

  let {
    agents,
    relays,
    connections = new Map(),
    workspaceDisclosure = $bindable<Record<string, boolean>>({}),
    responding,
    onopen,
  }: {
    agents: Agent[];
    relays: RelayConfig[];
    connections?: Map<string, RelayConnectionView>;
    workspaceDisclosure?: Record<string, boolean>;
    responding: Set<string>;
    onopen: (agent: Agent) => void;
  } = $props();

  const unavailableRelays = $derived(relays.filter((relay) => {
    const connection = connections.get(relay.id);
    return connection?.status === 'connected' && connection.inventory.state === 'error';
  }));
  const startingRelays = $derived(relays.filter((relay) => {
    const connection = connections.get(relay.id);
    return connection?.status === 'connected' && connection.inventory.state === 'starting';
  }));
  const readyRelays = $derived(relays.filter((relay) => {
    const connection = connections.get(relay.id);
    return connection?.status === 'connected' && connection.inventory.state === 'ready';
  }));

  const attentionDefinitions = [
    ['attention', 'Needs inspection', 'warning'],
    ['blocked', 'Needs input', 'danger'],
  ] as const;
  let relativeNow = $state(Date.now());
  const workspaces = $derived(workspaceGroups(agents.filter((agent) => {
    const group = agentStatusGroup(agent);
    return group !== 'blocked' && group !== 'attention';
  })));

  function rememberWorkspaceDisclosure(key: string, event: Event) {
    const details = event.currentTarget;
    if (!(details instanceof HTMLDetailsElement)) return;
    workspaceDisclosure[key] = details.open;
  }

  async function respond(agent: Agent, index: number, total: number, option: string) {
    await relayStore.respond(agent, index, total, option);
  }

  function agentMeta(agent: Agent, compact: boolean): string {
    const tab = compact ? '' : tabName(agent);
    const labels = [
      tab && tab !== displayName(agent) ? tab : '',
      agent.session || '',
    ].filter(Boolean);
    if (agent.agent && !hasAgentLogo(agent.agent)) labels.push(agent.agent);
    return labels.join(' · ');
  }

  function relativeTimestamp(timestamp: number): string {
    if (!timestamp) return '';
    const seconds = Math.max(0, Math.floor((relativeNow - timestamp) / 1_000));
    if (seconds < 60) return 'now';
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes}m`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours}h`;
    const days = Math.floor(hours / 24);
    if (days < 30) return `${days}d`;
    const months = Math.floor(days / 30);
    if (months < 12) return `${months}mo`;
    return `${Math.floor(months / 12)}y`;
  }

  function relativeAge(agent: Agent): string {
    return relativeTimestamp(agentLastActiveAt(agent));
  }

  onMount(() => {
    const timer = setInterval(() => { relativeNow = Date.now(); }, 60_000);
    return () => clearInterval(timer);
  });
</script>

{#snippet agentGrid(visible: Agent[], compact: boolean)}
  <div class:compact-agent-grid={compact} class="agent-grid">
    {#each visible as agent (agent.pane_id)}
      {@const interaction = questionInteraction(agent)}
      {@const options = approvalOptions(agent)}
      {@const group = agentStatusGroup(agent)}
      {@const tone = agentStatusTone(agent)}
      {@const blocked = group === 'blocked'}
      {@const needsInspection = group === 'attention'}
      {@const meta = agentMeta(agent, compact)}
      {@const age = relativeAge(agent)}
      {@const inventoryReady = !connections.has(agent.relay_id) || connections.get(agent.relay_id)?.inventory.state === 'ready'}
      <article class:blocked class:compact-agent-card={compact} class:stale={!inventoryReady} class="agent-card">
        <button
          class="agent-open"
          aria-label={`Open ${displayName(agent)} on ${hostLabel(agent)}`}
          disabled={!inventoryReady}
          title={!inventoryReady ? 'This cached agent is unavailable until Herdr inventory recovers.' : undefined}
          onclick={() => onopen(agent)}
        >
          <span class="agent-identity">
            <AgentLogo agent={agent.agent} />
            <span class={`status-dot status-${tone}`} class:hollow={group === 'ready'} aria-hidden="true"></span>
          </span>
          <span class="agent-copy">
            <span class="agent-title-row">
              <span class="agent-project">{displayName(agent)} <span class="host-badge">@{hostLabel(agent)}</span></span>
              {#if compact && age}
                <time class="agent-age" datetime={new Date(agentLastActiveAt(agent)).toISOString()} title={new Date(agentLastActiveAt(agent)).toLocaleString()}>{age}</time>
              {/if}
            </span>
            {#if meta}<span class="agent-meta">{meta}</span>{/if}
            {#if blocked || needsInspection}
              <span class="prompt-preview">{interaction?.question || approvalPromptPreview(agent)}</span>
            {/if}
          </span>
        </button>
        {#if blocked && !responding.has(agent.pane_id)}
          <div class="agent-actions" aria-label={`Actions for ${displayName(agent)}`}>
            {#if interaction}
              <Button variant="trust" size="sm" onclick={() => onopen(agent)}>
                {interaction.kind === 'multi_select' ? 'Choose options' : 'Choose answer'} ({interaction.options.length})
              </Button>
            {:else}
              {#each options as option, index (`${index}:${option}`)}
                <Button
                  variant={approvalButtonTone(option, index, options.length) === 'deny' ? 'danger' : approvalButtonTone(option, index, options.length) === 'trust' ? 'trust' : 'default'}
                  size="sm"
                  disabled={!inventoryReady}
                  title={!inventoryReady ? 'Agent controls are unavailable until Herdr inventory recovers.' : undefined}
                  onclick={() => respond(agent, index, options.length, option)}
                >{option.length > 48 ? `${option.slice(0, 45)}...` : option}</Button>
              {/each}
            {/if}
          </div>
        {:else if blocked}
          <p class="responding" role="status">Waiting for agent…</p>
        {/if}
      </article>
    {/each}
  </div>
{/snippet}

<main class="agent-list" aria-label="Agents">
  {#each unavailableRelays as relay (relay.id)}
    {@const inventory = connections.get(relay.id)?.inventory}
    <section class="inventory-warning" role="status" aria-label={`${relay.label} agent inventory unavailable`}>
      <strong>{relay.label} is connected, but its Herdr agent inventory is unavailable.</strong>
      <span>{inventory?.message || 'Refresh after checking Herdr on that computer.'}</span>
      {#if inventory?.stale}<span>Previously reported agents are shown as stale.</span>{/if}
    </section>
  {/each}

  {#if !agents.length && !relays.length}
    <div class="empty-state">
      <span class="empty-icon" aria-hidden="true">🐑</span>
      <h2>Herdr Mobile Relay</h2>
      <p>Monitor and approve agents from your phone.</p>
      <ol>
        <li>Run a relay on each computer.</li>
        <li>Give each computer its own <code>wss://</code> URL.</li>
        <li>Open Settings and add each relay.</li>
      </ol>
    </div>
  {:else if !agents.length && startingRelays.length}
    <div class="empty-state" role="status">Loading agents…</div>
  {:else if !agents.length && readyRelays.length}
    <div class="empty-state" role="status">No chat agents are running.</div>
  {:else if !agents.length && !unavailableRelays.length}
    <div class="empty-state" role="status">Waiting for relays…</div>
  {/if}

  {#each attentionDefinitions as [group, title, tone] (group)}
    {@const visible = sortedAgents(agents.filter((agent) => agentStatusGroup(agent) === group))}
    {#if visible.length}
      <section class="agent-section" aria-labelledby={`section-${group}`}>
        <h2 id={`section-${group}`} class="section-heading">
          <span class={`status-dot status-${tone}`}></span>{title}
          <span class="section-count" aria-hidden="true">{visible.length}</span>
        </h2>
        {@render agentGrid(visible, false)}
      </section>
    {/if}
  {/each}

  {#if workspaces.length}
    <section class="agent-section workspace-section" aria-labelledby="workspace-section-title">
      <h2 id="workspace-section-title" class="section-heading">
        Workspaces
        <span class="section-count" aria-hidden="true">{workspaces.length}</span>
      </h2>
      <div class="workspace-grid">
        {#each workspaces as workspace (workspace.key)}
          <details
            class="workspace-card"
            open={workspaceDisclosure[workspace.key] ?? (workspaces.length === 1)}
            ontoggle={(event) => rememberWorkspaceDisclosure(workspace.key, event)}
          >
            <summary>
              <span class="workspace-card-copy">
                <strong>{workspace.label}</strong>
                <small>{[workspace.cwd, `@${workspace.host}`].filter(Boolean).join(' · ')}</small>
              </span>
              <span class="workspace-counts" aria-label={`${workspace.tabs.length} tabs and ${workspace.agents.length} agents`}>
                {#if workspace.attentionCount}<em class="workspace-attention">{workspace.attentionCount} need you</em>{/if}
                <span>{workspace.tabs.length} {workspace.tabs.length === 1 ? 'tab' : 'tabs'}</span>
                <span>{workspace.agents.length} {workspace.agents.length === 1 ? 'agent' : 'agents'}</span>
                {#if workspace.lastActiveAt}
                  <time
                    datetime={new Date(workspace.lastActiveAt).toISOString()}
                    title={`Last agent activity: ${new Date(workspace.lastActiveAt).toLocaleString()}`}
                    aria-label={`Last agent activity ${new Date(workspace.lastActiveAt).toLocaleString()}`}
                  >{relativeTimestamp(workspace.lastActiveAt)}</time>
                {/if}
              </span>
            </summary>
            <div class="workspace-tabs">
              {#each workspace.tabs as tab (tab.id)}
                <section class="workspace-tab" aria-label={`${tab.label} tab`}>
                  <h3>{tab.label}</h3>
                  {@render agentGrid(tab.agents, true)}
                </section>
              {/each}
            </div>
          </details>
        {/each}
      </div>
    </section>
  {/if}
</main>

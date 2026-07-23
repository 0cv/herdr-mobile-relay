<script lang="ts">
  import Button from '$components/ui/Button.svelte';
  import {
    agentStatusGroup,
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

  let {
    agents,
    relays,
    connections = new Map(),
    responding,
    onopen,
  }: {
    agents: Agent[];
    relays: RelayConfig[];
    connections?: Map<string, RelayConnectionView>;
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

  const definitions = [
    ['blocked', 'Blocked', 'danger'],
    ['done', 'Done', 'success'],
    ['working', 'Working', 'warning'],
    ['ready', 'Idle', 'muted'],
    ['other', 'Other', 'muted'],
  ] as const;

  async function respond(agent: Agent, index: number, total: number, option: string) {
    await relayStore.respond(agent, index, total, option);
  }
</script>

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

  {#each definitions as [group, title, tone] (group)}
    {@const visible = sortedAgents(agents.filter((agent) => agentStatusGroup(agent) === group))}
    {#if visible.length}
      <section class="agent-section" aria-labelledby={`section-${group}`}>
        <h2 id={`section-${group}`} class="section-heading">
          <span class={`status-dot status-${tone}`} class:hollow={group === 'ready'}></span>{title}
        </h2>
        <div class="agent-grid">
          {#each visible as agent (agent.pane_id)}
            {@const interaction = questionInteraction(agent)}
            {@const options = approvalOptions(agent)}
            {@const blocked = group === 'blocked'}
            {@const tab = tabName(agent)}
          {@const inventoryReady = !connections.has(agent.relay_id) || connections.get(agent.relay_id)?.inventory.state === 'ready'}
          <article class:blocked class:stale={!inventoryReady} class="agent-card">
              <button
                class="agent-open"
                aria-label={`Open ${displayName(agent)} on ${hostLabel(agent)}`}
                disabled={!inventoryReady}
                title={!inventoryReady ? 'This cached agent is unavailable until Herdr inventory recovers.' : undefined}
                onclick={() => onopen(agent)}
              >
                <span class={`status-dot status-${tone}`} class:hollow={group === 'ready'}></span>
                <span class="agent-copy">
                  <span class="agent-project">{displayName(agent)} <span class="host-badge">@{hostLabel(agent)}</span></span>
                  <span class="agent-meta">{tab && tab !== displayName(agent) ? `${tab} · ` : ''}{agent.session ? `${agent.session} · ` : ''}{agent.agent || 'agent'}</span>
                  {#if blocked}
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
      </section>
    {/if}
  {/each}
</main>

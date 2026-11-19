<script lang="ts">
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import { navigate } from '$lib/router';
  import { relayStore } from '$lib/store';
  import type { RelayWorkspace } from '$lib/types';
  import { relayWorkspaceTrees, workspaceProvenance } from '$lib/workspaces';

  let { readOnlyRelayIds }: { readOnlyRelayIds: Set<string> } = $props();
  const { relayConfigs, connections, workspaces } = relayStore;
  const readyRelays = $derived($relayConfigs.filter(({ id }) => {
    const connection = $connections.get(id);
    return connection?.status === 'connected' && connection.inventory.state === 'ready';
  }));
  let selected = $state('');
  const relayId = $derived(readyRelays.find(({ id }) => id === selected)?.id ?? readyRelays[0]?.id ?? '');
  const trees = $derived(relayWorkspaceTrees($workspaces.filter((workspace) => workspace.relay_id === relayId)));
  const canLaunch = $derived(!readOnlyRelayIds.has(relayId) && $connections.get(relayId)?.capabilities.includes('agent_launch'));
</script>

{#snippet row(workspace: RelayWorkspace)}
  {@const cwd = workspace.cwd || workspace.worktree?.checkout_path}
  {@const provenance = workspaceProvenance(workspace)}
  <article class="workspace-management-row">
    <div>
      <h3>{workspace.label}</h3>
      {#if cwd}<p class="workspace-management-meta">{cwd}</p>{/if}
      {#if provenance}<p class="workspace-management-meta">{provenance}</p>{/if}
    </div>
    {#if canLaunch && cwd}
      <div class="workspace-management-actions">
        <Button size="sm" onclick={() => navigate({ view: 'launch', relayId, workspaceId: workspace.workspace_id, cwd })}>Start Agent</Button>
      </div>
    {/if}
  </article>
{/snippet}

<main class="page workspace-manager-page" aria-labelledby="workspace-manager-title">
  <div class="workspace-manager-heading">
    <h2 id="workspace-manager-title">Workspaces</h2>
    <p>Browse sample workspaces and start sample agents on Atlas. Other workspace and worktree changes are unavailable in the demo.</p>
  </div>
  <Card>
    <div class="form-stack">
      <label for="workspace-relay">Computer</label>
      <select id="workspace-relay" value={relayId} onchange={(event) => { selected = event.currentTarget.value; }}>
        {#if !readyRelays.length}<option value="">No sample computers ready</option>{/if}
        {#each readyRelays as relay (relay.id)}<option value={relay.id}>{relay.label}</option>{/each}
      </select>
    </div>
  </Card>
  <section class="workspace-management-list" aria-label="Herdr workspaces">
    {#each trees as { workspace, children } (workspace.workspace_id)}
      <Card>
        {@render row(workspace)}
        {#if children.length}
          <div class="workspace-management-worktrees" aria-label={`${workspace.label} linked worktrees`}>
            {#each children as child (child.workspace_id)}{@render row(child)}{/each}
          </div>
        {/if}
      </Card>
    {/each}
  </section>
</main>

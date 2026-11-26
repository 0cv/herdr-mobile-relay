<script lang="ts">
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import ManageKeyRow from '$components/ManageKeyRow.svelte';
  import { isAgentView, paneViewPreferenceKey } from '$lib/agent-view';
  import { displayName, hostLabel } from '$lib/agents';
  import { defaultAgentView, paneAgentViewOverrides, setPaneAgentView } from '$lib/preferences';
  import type { Agent } from '$lib/types';

  let { open = $bindable(false), agent }: { open: boolean; agent: Agent | null; readOnly?: boolean } = $props();
  const key = $derived(agent ? paneViewPreferenceKey(agent) : null);
  const preference = $derived(key ? $paneAgentViewOverrides[key] : undefined);

  function changeView(event: Event & { currentTarget: HTMLSelectElement }) {
    const value = event.currentTarget.value;
    if (agent && (value === 'default' || isAgentView(value))) setPaneAgentView(agent, value === 'default' ? null : value);
  }
</script>

<AppDialog id="manage-agent-dialog" bind:open title="Manage Agent" description={agent ? `${displayName(agent)} @${hostLabel(agent)}` : 'Agent unavailable'}>
  <ManageKeyRow>
    {#snippet children(editKeyRow)}
      <div class="form-stack">
        <label for="pane-default-view">Default View</label>
        <select id="pane-default-view" aria-describedby="pane-default-view-help" value={preference ?? 'default'} disabled={!key} onchange={changeView}>
          <option value="default">Use default ({$defaultAgentView === 'conversation' ? 'Conversation' : 'Terminal'})</option>
          <option value="terminal">Terminal</option>
          <option value="conversation">Conversation</option>
        </select>
        <p id="pane-default-view-help" class="hint">Applies the next time you open this pane during the visit.</p>
        <p class="hint">Renaming, clearing and stopping agents are unavailable in the demo.</p>
        <div class="dialog-actions">
          {@render editKeyRow()}
          <Button variant="ghost" onclick={() => { open = false; }}>Close</Button>
        </div>
      </div>
    {/snippet}
  </ManageKeyRow>
</AppDialog>

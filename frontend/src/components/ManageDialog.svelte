<script lang="ts">
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import { clientPaneId, displayName, hostLabel, tabName } from '$lib/agents';
  import { replaceView } from '$lib/router';
  import { relayStore } from '$lib/store';
  import type { Agent } from '$lib/types';

  let { open = $bindable(false), agent }: { open?: boolean; agent: Agent | null } = $props();
  let name = $state('');
  let confirming = $state<'clear' | 'stop' | ''>('');
  let busy = $state(false);
  let initializedPaneId = '';

  $effect(() => {
    if (!open) {
      initializedPaneId = '';
      return;
    }
    if (!agent || initializedPaneId === agent.pane_id) return;
    initializedPaneId = agent.pane_id;
    name = tabName(agent) || String(agent.project || '');
    confirming = '';
  });

  async function rename() {
    if (!agent || !name.trim()) {
      relayStore.showToast('Enter a name for the agent.', true);
      return;
    }
    busy = true;
    try {
      await relayStore.sendToAgent(agent, { type: 'agent_rename', name: name.trim() });
      open = false;
      relayStore.showToast(`Agent renamed to ${name.trim()}.`);
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
    } finally {
      busy = false;
    }
  }

  async function clearAgent() {
    if (!agent) return;
    if (confirming !== 'clear') {
      confirming = 'clear';
      return;
    }
    busy = true;
    try {
      const result = await relayStore.sendToAgent(agent, { type: 'agent_clear' }, 25_000);
      const warning = String(result.data?.warning || '');
      relayStore.showToast(warning || 'Agent cleared.', Boolean(warning));
      const rawPaneId = String(result.data?.pane_id || '');
      const replacement = await relayStore.waitForAgent(agent.relay_id, {
        rawPaneId,
        name: String(result.data?.name || ''),
        cwd: String(result.data?.cwd || agent.cwd || ''),
      });
      open = false;
      const paneId = replacement?.pane_id || (rawPaneId ? clientPaneId(agent.relay_id, rawPaneId) : '');
      if (paneId) replaceView({ view: 'terminal', paneId });
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
    } finally {
      busy = false;
    }
  }

  async function stopAgent() {
    if (!agent) return;
    if (confirming !== 'stop') {
      confirming = 'stop';
      return;
    }
    busy = true;
    try {
      await relayStore.sendToAgent(agent, { type: 'agent_stop' });
      open = false;
      relayStore.showToast('Agent stopped.');
      replaceView({ view: 'agents' });
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
    } finally {
      busy = false;
    }
  }
</script>

<AppDialog id="manage-agent-dialog" bind:open title="Manage Agent" description={agent ? `${displayName(agent)} @${hostLabel(agent)}` : 'Agent unavailable'}>
  <div class="form-stack">
    <label for="manage-name">New name</label>
    <input id="manage-name" bind:value={name} maxlength="80" autocomplete="off" />
    <div class="dialog-actions">
      <Button disabled={busy} onclick={rename}>Rename</Button>
      <Button variant="secondary" disabled={busy} onclick={clearAgent}>{confirming === 'clear' ? 'Confirm Clear' : 'Clear Agent'}</Button>
      <Button variant="danger" disabled={busy} onclick={stopAgent}>{confirming === 'stop' ? 'Confirm Stop' : 'Stop Agent'}</Button>
      <Button variant="ghost" disabled={busy} onclick={() => { open = false; }}>Cancel</Button>
    </div>
    {#if confirming}<p class="warning" role="alert">{confirming === 'stop' ? 'Tap Confirm Stop to close this agent pane.' : 'Tap Confirm Clear to start a fresh agent in the same working directory.'}</p>{/if}
  </div>
</AppDialog>

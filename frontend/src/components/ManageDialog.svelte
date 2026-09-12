<script lang="ts">
  import { tick } from 'svelte';
  import { get } from 'svelte/store';
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import { agentOpeningView, isAgentView, paneViewPreferenceKey } from '$lib/agent-view';
  import { displayName, hostLabel, sessionName, tabName } from '$lib/agents';
  import { defaultAgentView, paneAgentViewOverrides, setPaneAgentView } from '$lib/preferences';
  import { replaceView } from '$lib/router';
  import { relayStore } from '$lib/store';
  import type { Agent } from '$lib/types';

  let { open = $bindable(false), agent, readOnly = false }: { open?: boolean; agent: Agent | null; readOnly?: boolean } = $props();
  let name = $state('');
  let renameMode = $state<'tab' | 'session' | ''>('');
  let confirming = $state<'clear' | 'stop' | ''>('');
  let busy = $state(false);
  let initializedPaneId = '';
  let nameInput = $state<HTMLInputElement | null>(null);
  let actionMenu = $state<HTMLDivElement | null>(null);
  let confirmPanel = $state<HTMLDivElement | null>(null);

  const connections = relayStore.connections;
  const panePreferenceKey = $derived(agent ? paneViewPreferenceKey(agent) : null);
  const paneViewSelection = $derived.by(() => {
    const override = panePreferenceKey ? $paneAgentViewOverrides[panePreferenceKey] : undefined;
    return isAgentView(override) ? override : 'default';
  });
  const inheritedPaneViewLabel = $derived(`Use default (${$defaultAgentView === 'conversation' ? 'Conversation' : 'Terminal'})`);

  const sessionRenameAvailable = $derived(Boolean(
    agent && String(agent.agent || '').trim().toLocaleLowerCase().replace(/[\s_-]+/g, '') !== 'opencode',
  ));

  $effect(() => {
    if (!open) {
      initializedPaneId = '';
      renameMode = '';
      confirming = '';
      return;
    }
    if (!agent || initializedPaneId === agent.pane_id) return;
    initializedPaneId = agent.pane_id;
    name = '';
    renameMode = '';
    confirming = '';
  });

  $effect(() => {
    if (renameMode === 'session' && !sessionRenameAvailable) renameMode = '';
  });

  async function beginRename(mode: 'tab' | 'session') {
    if (!agent || readOnly || (mode === 'session' && !sessionRenameAvailable)) return;
    confirming = '';
    renameMode = mode;
    name = mode === 'tab'
      ? tabName(agent) || String(agent.project || '')
      : sessionName(agent);
    await tick();
    nameInput?.focus();
    nameInput?.select();
  }

  async function cancelRename() {
    const mode = renameMode;
    renameMode = '';
    name = '';
    await tick();
    actionMenu?.querySelector<HTMLButtonElement>(`[data-rename-action="${mode}"]`)?.focus();
  }

  // The confirmation replaces the menu, so the destructive button cannot be hit
  // by muscle memory. Focus lands on Cancel: it is the safe answer, and Enter
  // must never be what clears or stops an agent.
  async function beginConfirm(mode: 'clear' | 'stop') {
    if (readOnly) return;
    confirming = mode;
    await tick();
    confirmPanel?.querySelector<HTMLButtonElement>('[data-confirm-cancel]')?.focus();
  }

  async function cancelConfirm() {
    const mode = confirming;
    confirming = '';
    await tick();
    actionMenu?.querySelector<HTMLButtonElement>(`[data-confirm-action="${mode}"]`)?.focus();
  }

  function changePaneView(event: Event): void {
    const select = event.currentTarget as HTMLSelectElement;
    const previous = paneViewSelection;
    const selected = select.value;
    if (!agent || (selected !== 'default' && !isAgentView(selected))) {
      select.value = previous;
      return;
    }
    const result = setPaneAgentView(agent, selected === 'default' ? null : selected);
    if (result === 'saved') return;
    select.value = previous;
    relayStore.showToast(result === 'invalid-target'
      ? 'This pane does not have a stable identity yet.'
      : 'Could not save this pane’s default view on this device.', true);
  }

  function nextName(): string | null {
    const value = name.trim();
    if (!value) {
      relayStore.showToast('Enter a new name.', true);
      return null;
    }
    return value;
  }

  async function renameTab() {
    if (!agent || readOnly) return;
    const value = nextName();
    if (!value) return;
    busy = true;
    try {
      await relayStore.sendToAgent(agent, { type: 'agent_rename', name: value });
      open = false;
      relayStore.showToast(`Tab renamed to ${value}.`);
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
    } finally {
      busy = false;
    }
  }

  async function renameSession() {
    if (!agent || readOnly || !sessionRenameAvailable) return;
    const value = nextName();
    if (!value) return;
    busy = true;
    try {
      await relayStore.sendToAgent(agent, { type: 'submit_prompt', text: `/rename ${value}` });
      open = false;
      relayStore.showToast(`Session rename sent for ${value}.`);
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
    } finally {
      busy = false;
    }
  }

  async function clearAgent() {
    if (!agent || readOnly) return;
    if (confirming !== 'clear') {
      void beginConfirm('clear');
      return;
    }
    busy = true;
    try {
      const result = await relayStore.sendToAgent(agent, { type: 'agent_clear' }, 45_000);
      const warning = String(result.data?.warning || '');
      relayStore.showToast(warning || 'Agent cleared.', Boolean(warning));
      const rawPaneId = String(result.data?.pane_id || '');
      const replacement = await relayStore.waitForAgent(agent.relay_id, {
        rawPaneId,
        name: String(result.data?.name || ''),
        cwd: String(result.data?.cwd || agent.cwd || ''),
      });
      open = false;
      if (replacement) {
        replaceView(agentOpeningView(
          replacement,
          get(connections).get(replacement.relay_id),
          get(defaultAgentView),
          get(paneAgentViewOverrides),
        ));
      }
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
    } finally {
      busy = false;
    }
  }

  async function stopAgent() {
    if (!agent || readOnly) return;
    if (confirming !== 'stop') {
      void beginConfirm('stop');
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
  {#if renameMode}
    <form class="form-stack" onsubmit={(event) => {
      event.preventDefault();
      if (renameMode === 'tab') void renameTab();
      else void renameSession();
    }}>
      <label for="manage-name">New {renameMode} name</label>
      <input bind:this={nameInput} id="manage-name" bind:value={name} required autocomplete="off" />
      <div class="dialog-actions">
        <Button type="submit" disabled={busy || readOnly}>Rename</Button>
        <Button variant="ghost" disabled={busy} onclick={cancelRename}>Cancel</Button>
      </div>
    </form>
  {:else if confirming}
    <div bind:this={confirmPanel} class="form-stack">
      <p class="warning" role="alert">
        {confirming === 'stop'
          ? 'Stop this agent? Its pane closes on the computer.'
          : 'Clear this agent? A fresh agent starts in the same working directory.'}
      </p>
      <div class="dialog-actions">
        <Button
          variant={confirming === 'stop' ? 'danger' : 'secondary'}
          disabled={busy || readOnly}
          onclick={confirming === 'stop' ? stopAgent : clearAgent}
        >{confirming === 'stop' ? 'Confirm Stop' : 'Confirm Clear'}</Button>
        <Button variant="ghost" data-confirm-cancel="true" disabled={busy} onclick={cancelConfirm}>Cancel</Button>
      </div>
    </div>
  {:else}
    <div bind:this={actionMenu} class="form-stack">
      <label for="pane-default-view">Default View</label>
      <select
        id="pane-default-view"
        aria-describedby="pane-default-view-help"
        value={paneViewSelection}
        disabled={busy || !panePreferenceKey}
        onchange={changePaneView}
      >
        <option value="default">{inheritedPaneViewLabel}</option>
        <option value="terminal">Terminal</option>
        <option value="conversation">Conversation</option>
      </select>
      <p id="pane-default-view-help" class="hint">
        {#if panePreferenceKey}
          Only affects this pane on this device. Applied the next time you open it.
        {:else}
          A stable pane identity is unavailable, so this preference cannot be saved yet.
        {/if}
      </p>
      <div class="dialog-actions">
        <Button data-rename-action="tab" disabled={busy || readOnly} onclick={() => beginRename('tab')}>Rename Tab</Button>
        {#if sessionRenameAvailable}
          <Button data-rename-action="session" variant="secondary" disabled={busy || readOnly} onclick={() => beginRename('session')}>Rename Session</Button>
        {/if}
        <Button data-confirm-action="clear" variant="secondary" disabled={busy || readOnly} onclick={clearAgent}>Clear Agent</Button>
        <Button data-confirm-action="stop" variant="danger" disabled={busy || readOnly} onclick={stopAgent}>Stop Agent</Button>
        <Button variant="ghost" disabled={busy} onclick={() => { open = false; }}>Close</Button>
      </div>
    </div>
  {/if}
</AppDialog>

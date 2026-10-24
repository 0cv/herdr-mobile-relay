<script lang="ts">
  import { untrack } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import { inputContextIdentity } from '$lib/input-target-state';
  import { terminalInputDelivery } from '$lib/terminal-input-delivery.svelte';
  import { piPromptDelivery } from '$lib/pi-prompt.svelte';
  import { securityState } from '$lib/security';
  import { relayStore } from '$lib/store';
  import type { Agent, TerminalFrame } from '$lib/types';
  import type ManualTerminalInput from '$components/ManualTerminalInput.svelte';

  let { agent, frame, readOnly = false, busy = false, manual = $bindable(false), blocked = $bindable(false) }:
    { agent: Agent; frame?: TerminalFrame; readOnly?: boolean; busy?: boolean; manual?: boolean; blocked?: boolean } = $props();
  const connections = relayStore.connections;
  let Panel = $state<typeof ManualTerminalInput>();
  let loading = $state(false);
  const identity = $derived(inputContextIdentity(agent));
  const delivery = $derived(terminalInputDelivery(agent));
  const piDelivery = $derived(piPromptDelivery(agent));
  const context = $derived(JSON.stringify([identity, $connections.get(agent.relay_id)?.status, $securityState.locked, readOnly]));

  $effect.pre(() => {
    void context;
    untrack(() => { manual = false; });
  });
  $effect.pre(() => {
    const state = delivery;
    const piState = piDelivery;
    state.retain();
    piState.retain();
    return () => { state.release(); piState.release(); };
  });
  $effect.pre(() => {
    const next = delivery.uncertain || delivery.pending || piDelivery.pending || piDelivery.uncertain;
    if (untrack(() => blocked) !== next) blocked = next;
  });

  async function enter() {
    if (readOnly || busy || loading) return;
    const started = context;
    loading = true;
    try {
      Panel = (await import('$components/ManualTerminalInput.svelte')).default;
      if (context === started) manual = true;
    } catch {
      relayStore.showToast('Could not load controls.', true);
    } finally {
      loading = false;
    }
  }
</script>

{#if !readOnly}
  <div hidden={manual}>
    <Button variant="secondary" size="sm" disabled={busy || loading} onclick={enter}>Type in terminal</Button>
  </div>
  {#if Panel}
    {#key identity}
      <Panel {agent} {frame} {readOnly} {delivery} active={manual} onexit={() => { manual = false; }} />
    {/key}
  {/if}
  {#if !manual && delivery.uncertain}
    <p role="alert">Delivery is uncertain. You can edit drafts; reopen Type in terminal to inspect before sending.</p>
  {:else if !manual && delivery.pending}
    <p role="status">Waiting for terminal input delivery.</p>
  {/if}
{/if}

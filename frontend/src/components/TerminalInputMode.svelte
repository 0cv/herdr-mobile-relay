<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import KeyboardIcon from '$components/ui/KeyboardIcon.svelte';
  import { inputContextIdentity } from '$lib/input-target-state';
  import { terminalInputDelivery } from '$lib/terminal-input-delivery.svelte';
  import { piPromptDelivery } from '$lib/pi-prompt.svelte';
  import { securityState } from '$lib/security';
  import { relayStore } from '$lib/store';
  import { AppReloadHeld, importOrReload } from '$lib/updates';
  import type { Agent, TerminalFrame } from '$lib/types';
  import type ManualTerminalInput from '$components/ManualTerminalInput.svelte';

  // The parent renders the trigger (inline with the terminal keys) and calls enter(); this component owns the
  // lazily loaded panel and the delivery notices.
  let { agent, frame, readOnly = false, busy = false, manual = $bindable(false), blocked = $bindable(false), loading = $bindable(false), returnFocus }:
    { agent: Agent; frame?: TerminalFrame; readOnly?: boolean; busy?: boolean; manual?: boolean; blocked?: boolean; loading?: boolean; returnFocus?: () => void } = $props();
  const connections = relayStore.connections;
  let Panel = $state<typeof ManualTerminalInput>();
  let destroyed = false;
  onDestroy(() => { destroyed = true; });
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

  export async function enter() {
    if (readOnly || busy || loading) return;
    const started = context;
    loading = true;
    try {
      Panel = (await importOrReload(
        () => import('$components/ManualTerminalInput.svelte'),
        () => !destroyed && context === started,
      )).default;
      if (context === started) manual = true;
    } catch (error) {
      relayStore.showToast(error instanceof AppReloadHeld ? error.message : 'Could not load controls.', true);
    } finally {
      loading = false;
    }
  }
</script>

{#if !readOnly}
  {#if Panel}
    {#key identity}
      <Panel {agent} {frame} {readOnly} {delivery} active={manual} onexit={() => { manual = false; }} {returnFocus} />
    {/key}
  {/if}
  {#if !manual && delivery.uncertain}
    <p role="alert">Delivery is uncertain. Tap <KeyboardIcon label="Keyboard" inline /> to check the terminal before sending.</p>
  {:else if !manual && delivery.pending}
    <p role="status">Waiting for terminal input delivery.</p>
  {/if}
{/if}

import { piPromptDelivery } from './pi-prompt.svelte';
import { terminalInputDelivery } from './terminal-input-delivery.svelte';
import type { Agent } from './types';

export function terminalInputGate(agent: () => Agent) {
  const delivery = $derived(terminalInputDelivery(agent()));
  const piDelivery = $derived(piPromptDelivery(agent()));
  $effect.pre(() => {
    const state = delivery;
    const piState = piDelivery;
    state.retain();
    piState.retain();
    return () => { state.release(); piState.release(); };
  });

  return {
    get delivery() { return delivery; },
    get piDelivery() { return piDelivery; },
    get blocked() { return delivery.pending || delivery.uncertain || piDelivery.pending || piDelivery.uncertain; },
  };
}

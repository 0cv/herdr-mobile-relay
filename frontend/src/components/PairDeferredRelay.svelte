<script lang="ts">
  import { tick } from 'svelte';
  import Button from '$components/ui/Button.svelte';

  let { relayId, onPair }: { relayId: string; onPair: (relayId: string) => boolean } = $props();
  let confirming = $state(false);
  let panel = $state<HTMLDivElement | null>(null);

  async function beginConfirm() {
    confirming = true;
    await tick();
    panel?.querySelector<HTMLButtonElement>('[data-pair-cancel]')?.focus();
  }

  async function cancelConfirm() {
    confirming = false;
    await tick();
    panel?.querySelector<HTMLButtonElement>('button')?.focus();
  }
</script>

<div bind:this={panel}>
  {#if confirming}
    <p class="warning" role="alert">
      This browser becomes its own device, listed and revocable separately. The link's
      one-use secret is spent here, so print or send a new one for the Home Screen app.
      Safari may clear this site's storage after a week without use; invite it again if
      that happens.
    </p>
    <div class="dialog-actions">
      <Button onclick={() => { onPair(relayId); confirming = false; }}>Confirm</Button>
      <Button variant="secondary" data-pair-cancel onclick={() => void cancelConfirm()}>Cancel</Button>
    </div>
  {:else}
    <Button variant="secondary" onclick={() => void beginConfirm()}>Pair this browser instead</Button>
  {/if}
</div>

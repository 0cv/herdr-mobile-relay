<script lang="ts">
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import { securityState, unlockWithDevice } from '$lib/security';
</script>

<AppDialog
  id="unlock-dialog"
  open={$securityState.locked}
  dismissible={false}
  title="Unlock Herdr"
  description={$securityState.reason === 'resume'
    ? 'Verify to reconnect relays after the pause.'
    : 'Verify with your device fingerprint, face unlock, or passcode.'}
>
  <Button disabled={$securityState.busy} onclick={() => unlockWithDevice($securityState.reason)}>Unlock</Button>
  {#if $securityState.status}<p class="form-status" role="status">{$securityState.status}</p>{/if}
</AppDialog>

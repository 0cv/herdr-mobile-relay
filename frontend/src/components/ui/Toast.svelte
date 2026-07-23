<script lang="ts">
  import { relayStore } from '$lib/store';
  const toast = relayStore.toast;

  let visible = $state(false);
  let timer: ReturnType<typeof setTimeout> | undefined;

  $effect(() => {
    if (!$toast) return;
    visible = true;
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => { visible = false; }, 4_000);
    return () => {
      if (timer) clearTimeout(timer);
      timer = undefined;
    };
  });
</script>

{#if $toast}
  <div class:visible class:error={$toast.error} class="toast" role="status" aria-live="polite">
    {$toast.message}
  </div>
{/if}

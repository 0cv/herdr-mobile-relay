<script lang="ts">
  import { relayStore } from '$lib/store';
  const toast = relayStore.toast;

  let element = $state<HTMLDivElement>();
  let visible = $state(false);
  let timer: ReturnType<typeof setTimeout> | undefined;
  let hideTimer: ReturnType<typeof setTimeout> | undefined;
  const supportsPopover = typeof HTMLElement !== 'undefined'
    && typeof HTMLElement.prototype.showPopover === 'function';

  $effect(() => {
    if (!$toast || !element) return;
    visible = true;
    if (timer) clearTimeout(timer);
    if (hideTimer) clearTimeout(hideTimer);
    if (supportsPopover && !element.matches(':popover-open')) element.showPopover();
    timer = setTimeout(() => {
      visible = false;
      if (supportsPopover) {
        hideTimer = setTimeout(() => {
          if (element?.matches(':popover-open')) element.hidePopover();
        }, 150);
      }
    }, 4_000);
    return () => {
      if (timer) clearTimeout(timer);
      if (hideTimer) clearTimeout(hideTimer);
      timer = undefined;
      hideTimer = undefined;
    };
  });
</script>

{#if $toast}
  <div bind:this={element} popover={supportsPopover ? 'manual' : undefined} class:visible class:error={$toast.error} class="toast" role="status" aria-live="polite">
    {$toast.message}
  </div>
{/if}

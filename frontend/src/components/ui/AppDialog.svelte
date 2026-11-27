<script lang="ts">
  import { tick, type Snippet } from 'svelte';

  let {
    id,
    open = $bindable(false),
    title,
    hideTitle = false,
    description = '',
    children,
    dismissible = true,
    onDismiss,
  }: {
    id: string;
    open?: boolean;
    title: string;
    hideTitle?: boolean;
    description?: string;
    children?: Snippet;
    dismissible?: boolean;
    onDismiss?: () => void;
  } = $props();

  let dialog = $state<HTMLDialogElement>();

  $effect(() => {
    if (!open || !dialog || dialog.open) return;
    dialog.showModal();
  });

  function cancel(event: Event) {
    if (!dismissible) {
      event.preventDefault();
      return;
    }
    dismiss();
  }

  function dismiss() {
    if (!open) return;
    open = false;
    if (onDismiss) void tick().then(onDismiss);
  }

  function dismissFromBackdrop(event: MouseEvent) {
    if (dismissible && event.target === dialog) dismiss();
  }
</script>

{#if open}
  <dialog
    bind:this={dialog}
    {id}
    class="app-dialog"
    aria-labelledby={`${id}-title`}
    aria-describedby={description ? `${id}-description` : undefined}
    oncancel={cancel}
    onclose={dismiss}
    onclick={dismissFromBackdrop}
  >
    <div class="dialog-content">
      <h2 class={hideTitle ? 'sr-only' : 'dialog-title'} id={`${id}-title`}>{title}</h2>
      {#if description}<p class="dialog-description" id={`${id}-description`}>{description}</p>{/if}
      {@render children?.()}
    </div>
  </dialog>
{/if}

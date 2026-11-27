<script lang="ts">
  import { tick, type Snippet } from 'svelte';
  import Button from '$components/ui/Button.svelte';

  let { children }: { children: Snippet<[Snippet]> } = $props();
  let Editor = $state<typeof import('./KeyRowEditor.svelte').default>();
  let loading = $state(false);
  let failed = $state(false);

  async function edit() {
    loading = true;
    failed = false;
    try {
      Editor = (await import('./KeyRowEditor.svelte')).default;
    } catch {
      failed = true;
    } finally {
      loading = false;
    }
  }

  async function close() {
    Editor = undefined;
    await tick();
    document.getElementById('edit-key-row')?.focus();
  }
</script>

{#snippet trigger()}
  <Button id="edit-key-row" variant="secondary" disabled={loading} onclick={edit}>Edit key row</Button>
{/snippet}

{#if Editor}
  <Editor {close} />
{:else}
  {#if failed}<p class="warning" role="alert">Could not open the key row editor.</p>{/if}
  {@render children(trigger)}
{/if}
